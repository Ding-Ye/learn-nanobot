package main

import (
	"sync"
	"testing"
	"time"
)

// stopOK is a one-line "FakeProvider Script entry that returns stop with
// the given text".
func stopOK(text string) LLMResponse {
	return LLMResponse{
		AssistantBlocks: []ContentBlock{{Type: "text", Text: text}},
		Content:         text,
		FinishReason:    "stop",
	}
}

// TestBus_TwoSessions_Parallel — two sessions with a slow FakeProvider
// each. Wall-clock time should be ~one-sleep, not ~two-sleeps, because
// the Bus runs each session in its own goroutine. Threshold is generous
// (180ms vs. 100ms+100ms=200ms baseline) so we don't false-fail under CI
// load.
func TestBus_TwoSessions_Parallel(t *testing.T) {
	provA := &FakeProvider{
		Script: []LLMResponse{stopOK("a-done")},
		Delay:  100 * time.Millisecond,
	}
	provB := &FakeProvider{
		Script: []LLMResponse{stopOK("b-done")},
		Delay:  100 * time.Millisecond,
	}
	// We need two providers because Script is a single shared sequence.
	// In a real Bus there's typically one provider; here we cheat by
	// giving each session its own Runner so the two FakeProviders don't
	// alias each other's Script index. The Bus normally takes one Runner;
	// to test parallelism we wire two different SessionManagers + Buses
	// that share the out channel via select.
	smA := NewSessionManager()
	smB := NewSessionManager()
	regA := NewRegistry()
	regB := NewRegistry()
	busA := NewBus(smA, NewRunner(provA), regA, "fake", 5)
	busB := NewBus(smB, NewRunner(provB), regB, "fake", 5)

	t0 := time.Now()
	busA.Send(InboundMessage{SessionKey: "sess-a", UserText: "hello-a", ID: "a1"})
	busB.Send(InboundMessage{SessionKey: "sess-b", UserText: "hello-b", ID: "b1"})

	// Wait for both replies.
	var wg sync.WaitGroup
	wg.Add(2)
	var aText, bText string
	go func() {
		defer wg.Done()
		aText = (<-busA.Out()).Text
	}()
	go func() {
		defer wg.Done()
		bText = (<-busB.Out()).Text
	}()
	wg.Wait()
	elapsed := time.Since(t0)

	busA.Stop()
	busB.Stop()

	if aText != "a-done" || bText != "b-done" {
		t.Fatalf("texts wrong: a=%q b=%q", aText, bText)
	}
	// 180ms threshold: serial would be ~200ms+, parallel ~100ms+overhead.
	if elapsed >= 180*time.Millisecond {
		t.Fatalf("two sessions took %v — expected parallel (<180ms)", elapsed)
	}
}

// TestBus_SameSession_Serial — three messages submitted to the same key
// should be processed in submission order. We assert order via the
// CorrelationID echoed on each OutboundMessage.
func TestBus_SameSession_Serial(t *testing.T) {
	prov := &FakeProvider{
		Script: []LLMResponse{
			stopOK("reply-1"),
			stopOK("reply-2"),
			stopOK("reply-3"),
		},
		// Small delay so a race-condition implementation would interleave;
		// our serial guarantee should still hold.
		Delay: 30 * time.Millisecond,
	}
	sm := NewSessionManager()
	reg := NewRegistry()
	bus := NewBus(sm, NewRunner(prov), reg, "fake", 5)

	bus.Send(InboundMessage{SessionKey: "k", UserText: "one", ID: "msg-1"})
	bus.Send(InboundMessage{SessionKey: "k", UserText: "two", ID: "msg-2"})
	bus.Send(InboundMessage{SessionKey: "k", UserText: "three", ID: "msg-3"})

	got := []string{}
	for i := 0; i < 3; i++ {
		out := <-bus.Out()
		if out.Err != nil {
			t.Fatalf("unexpected Err on out %d: %v", i, out.Err)
		}
		got = append(got, out.CorrelationID)
	}
	bus.Stop()

	want := []string{"msg-1", "msg-2", "msg-3"}
	if len(got) != len(want) {
		t.Fatalf("got %d outbounds, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordering broke: out[%d].CorrelationID = %q, want %q (got=%v)", i, got[i], want[i], got)
		}
	}

	// Sanity: after three serial turns, the session's message log has 6
	// entries: 3 user, 3 assistant.
	s, ok := sm.Get("k")
	if !ok {
		t.Fatalf("session k missing")
	}
	if got := len(s.Messages); got != 6 {
		t.Fatalf("session messages = %d, want 6 (3 user + 3 assistant)", got)
	}
}

// TestBus_StopBeforeProcessing_Drains — Stop is called while messages
// are still queued in the session's inbound chan. We expect:
//
//   - all queued messages to be processed (drained), not dropped silently
//   - Stop to return without deadlocking
//   - the out channel to be closed afterward
//
// This is the only test that explicitly exercises the goroutine-shutdown
// ordering invariant — race-detector failures here usually mean a missing
// mutex around b.in or b.done.
func TestBus_StopBeforeProcessing_Drains(t *testing.T) {
	prov := &FakeProvider{
		Script: []LLMResponse{
			stopOK("r1"), stopOK("r2"), stopOK("r3"),
		},
		Delay: 20 * time.Millisecond,
	}
	sm := NewSessionManager()
	reg := NewRegistry()
	bus := NewBus(sm, NewRunner(prov), reg, "fake", 5)

	// Pile up three messages on the same session.
	bus.Send(InboundMessage{SessionKey: "k", UserText: "1", ID: "1"})
	bus.Send(InboundMessage{SessionKey: "k", UserText: "2", ID: "2"})
	bus.Send(InboundMessage{SessionKey: "k", UserText: "3", ID: "3"})

	// Read in a goroutine so Stop's "wait for goroutines" doesn't
	// deadlock against a full out chan.
	got := []string{}
	done := make(chan struct{})
	go func() {
		for out := range bus.Out() {
			got = append(got, out.CorrelationID)
		}
		close(done)
	}()

	// Stop closes inbound chans; session loop drains and exits; Stop
	// returns; then it closes out, which makes the reader goroutine exit.
	bus.Stop()
	<-done

	if len(got) != 3 {
		t.Fatalf("Stop did not drain: got %d outbounds (%v), want 3", len(got), got)
	}
	for i, w := range []string{"1", "2", "3"} {
		if got[i] != w {
			t.Fatalf("drain order broke: got[%d] = %q, want %q", i, got[i], w)
		}
	}

	// Calling Stop a second time should be a no-op (must not panic).
	bus.Stop()
}
