package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// makeIdleSession builds a session with enough content to support a
// single consolidation round, and an UpdatedAt 100ms in the past so
// the TTL=50ms tests fire. The session is registered with the
// SessionManager so AutoCompact can find it via Get.
func makeIdleSession(t *testing.T, sm *SessionManager, key string, idle time.Duration) *Session {
	t.Helper()
	body := strings.Repeat("o", 80)
	sess := sm.GetOrCreate(key)
	sess.Messages = []Message{
		makeTextMsg("user", body),
		makeTextMsg("assistant", body),
		makeTextMsg("user", body),
		makeTextMsg("assistant", body),
		makeTextMsg("user", body),
	}
	sess.UpdatedAt = time.Now().Add(-idle)
	return sess
}

// waitFor polls fn() until it returns true or timeout elapses.
// Test helper; race-detector-safe.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fn()
}

// TestAutoCompact_IdleSession_GetsCompacted — register one session
// that's been idle 100ms; call CheckExpired with empty activeKeys; the
// goroutine should run Consolidate, write the summary file, and the
// AutoCompact's summaries map should hold the result.
func TestAutoCompact_IdleSession_GetsCompacted(t *testing.T) {
	mem := newTestStore(t)
	sm := NewSessionManager()
	makeIdleSession(t, sm, "cli:idle", 100*time.Millisecond)

	provider := &FakeProvider{Script: []LLMResponse{
		{Content: "compacted-by-bg", FinishReason: "stop"},
	}}
	cons := NewConsolidator(provider, mem)
	ac := NewAutoCompact(sm, cons, 50*time.Millisecond)

	queued := ac.CheckExpired(context.Background(), nil)
	if len(queued) != 1 || queued[0] != "cli:idle" {
		t.Fatalf("queued = %v, want [cli:idle]", queued)
	}

	if !waitFor(t, time.Second, func() bool {
		return ac.LastSummary("cli:idle") != ""
	}) {
		t.Fatalf("AutoCompact never recorded a summary; archiving=%v", ac.IsArchiving("cli:idle"))
	}

	if got := ac.LastSummary("cli:idle"); got != "compacted-by-bg" {
		t.Errorf("LastSummary = %q, want %q", got, "compacted-by-bg")
	}

	stored, err := mem.ReadSummary("cli:idle")
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if stored != "compacted-by-bg" {
		t.Errorf("on-disk summary = %q, want %q", stored, "compacted-by-bg")
	}

	// Goroutine should have cleared its archiving flag by now.
	if !waitFor(t, time.Second, func() bool { return !ac.IsArchiving("cli:idle") }) {
		t.Errorf("archiving flag never cleared after goroutine finished")
	}
}

// TestAutoCompact_ActiveSession_Skipped — same setup, but the session
// is in the activeKeys snapshot. CheckExpired must NOT queue it; no
// Consolidator goroutine runs.
func TestAutoCompact_ActiveSession_Skipped(t *testing.T) {
	mem := newTestStore(t)
	sm := NewSessionManager()
	makeIdleSession(t, sm, "cli:active", 100*time.Millisecond)

	provider := &FakeProvider{Script: []LLMResponse{
		{Content: "should-not-be-called", FinishReason: "stop"},
	}}
	cons := NewConsolidator(provider, mem)
	ac := NewAutoCompact(sm, cons, 50*time.Millisecond)

	active := map[string]bool{"cli:active": true}
	queued := ac.CheckExpired(context.Background(), active)
	if len(queued) != 0 {
		t.Errorf("queued = %v, want empty (session is active)", queued)
	}

	// Wait briefly to make sure no goroutine spawned. If the
	// FakeProvider were called we'd see a non-empty summary.
	time.Sleep(100 * time.Millisecond)

	if got := ac.LastSummary("cli:active"); got != "" {
		t.Errorf("LastSummary = %q, want empty (no consolidation should run)", got)
	}
	if provider.Calls() != 0 {
		t.Errorf("provider.Calls = %d, want 0 (active session must be skipped)", provider.Calls())
	}

	// Summary file should not exist on disk.
	stored, err := mem.ReadSummary("cli:active")
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if stored != "" {
		t.Errorf("on-disk summary present (%q); should be missing", stored)
	}
}

// TestAutoCompact_AlreadyArchiving_Skipped — call CheckExpired twice
// in rapid succession. The first call queues a goroutine; the second
// call must NOT queue another even though the session is still idle.
// We verify by counting Provider.Chat invocations: with a slow
// provider, exactly one Consolidator invocation should be observed
// across both CheckExpired calls.
func TestAutoCompact_AlreadyArchiving_Skipped(t *testing.T) {
	mem := newTestStore(t)
	sm := NewSessionManager()
	makeIdleSession(t, sm, "cli:racy", 100*time.Millisecond)

	// Slow the provider so the first goroutine is still in flight
	// when we call CheckExpired the second time.
	provider := newCounterProvider(LLMResponse{Content: "once", FinishReason: "stop"}, 80*time.Millisecond)
	cons := NewConsolidator(provider, mem)
	ac := NewAutoCompact(sm, cons, 50*time.Millisecond)

	q1 := ac.CheckExpired(context.Background(), nil)
	if len(q1) != 1 {
		t.Fatalf("first CheckExpired queued = %v, want 1", q1)
	}

	// Immediately call again; the in-flight goroutine should make
	// this a no-op.
	q2 := ac.CheckExpired(context.Background(), nil)
	if len(q2) != 0 {
		t.Errorf("second CheckExpired queued = %v, want empty (already archiving)", q2)
	}

	// Wait for the goroutine to drain.
	if !waitFor(t, 2*time.Second, func() bool { return !ac.IsArchiving("cli:racy") }) {
		t.Fatalf("archiving flag never cleared")
	}

	if got := provider.Count(); got != 1 {
		t.Errorf("provider.Count = %d, want 1 (only one goroutine should run)", got)
	}
}

// TestAutoCompact_RunLoop_StopsOnContextCancel — the Run loop is the
// production driver. Make sure it terminates cleanly when ctx is
// cancelled.
func TestAutoCompact_RunLoop_StopsOnContextCancel(t *testing.T) {
	mem := newTestStore(t)
	sm := NewSessionManager()
	provider := &FakeProvider{Script: []LLMResponse{}}
	cons := NewConsolidator(provider, mem)
	ac := NewAutoCompact(sm, cons, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ac.Run(ctx, 25*time.Millisecond, func() map[string]bool { return nil })
		close(done)
	}()

	// Let the ticker fire once or twice.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(time.Second):
		t.Errorf("Run did not return after ctx cancel")
	}
}
