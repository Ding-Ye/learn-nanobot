package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helpers ------------------------------------------------------------------

// makeTextMsg constructs a Message with one text content block. Used by
// the boundary-picker tests to set up sessions with predictable token
// counts. With the default `len(s)/4` estimator, a string of length 80
// is worth ~20 tokens.
func makeTextMsg(role, text string) Message {
	return Message{
		Role:    role,
		Content: []ContentBlock{{Type: "text", Text: text}},
	}
}

// makeToolResultMsg constructs a user-role message whose only content
// block is a tool_result. The boundary picker should NOT treat this as
// a legal cut point.
func makeToolResultMsg(toolUseID, body string) Message {
	return Message{
		Role: "user",
		Content: []ContentBlock{{
			Type:        "tool_result",
			ToolUseID:   toolUseID,
			ToolContent: body,
		}},
	}
}

// newTestStore builds a MemoryStore rooted at a fresh per-test temp
// dir. The Cleanup hook deletes the dir when the test ends.
func newTestStore(t *testing.T) *MemoryStore {
	t.Helper()
	dir := t.TempDir()
	mem, err := NewMemoryStore(dir)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	return mem
}

// stopOnceProvider returns a single LLMResponse from Chat then errors
// — useful when a test must guarantee Chat is invoked exactly once.
type stopOnceProvider struct {
	resp   LLMResponse
	calls  int
	delay  time.Duration
}

func (p *stopOnceProvider) Chat(ctx context.Context, _ ChatReq) (LLMResponse, error) {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return LLMResponse{}, ctx.Err()
		}
	}
	p.calls++
	if p.calls > 1 {
		return LLMResponse{}, errors.New("stopOnceProvider: called more than once")
	}
	return p.resp, nil
}

// counterProvider counts Chat invocations across goroutines for the
// AutoCompact race tests. Returns the same scripted response for every
// call.
type counterProvider struct {
	resp  LLMResponse
	delay time.Duration
	count int
	mu    chan struct{}
}

func newCounterProvider(resp LLMResponse, delay time.Duration) *counterProvider {
	return &counterProvider{resp: resp, delay: delay, mu: make(chan struct{}, 1)}
}

func (p *counterProvider) Chat(ctx context.Context, _ ChatReq) (LLMResponse, error) {
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return LLMResponse{}, ctx.Err()
		}
	}
	p.mu <- struct{}{}
	p.count++
	<-p.mu
	return p.resp, nil
}

func (p *counterProvider) Count() int {
	p.mu <- struct{}{}
	defer func() { <-p.mu }()
	return p.count
}

// tests --------------------------------------------------------------------

// TestConsolidator_PickBoundary_LegalUserTurnOnly — set up a session
// where the message immediately past the would-be boundary is an
// `assistant` (not a user). PickBoundary should walk past until it
// finds a real user message; if no such user exists, it returns false.
//
// Concretely: 6 messages — user/assistant/user/assistant/user/assistant.
// We ask for 30 tokens removed (each user/assistant pair is ~40 tokens
// with the 80-char content). The picker should land on idx=4 (the third
// user message), NOT idx=3 (the assistant) — because assistants are
// not legal turn boundaries.
func TestConsolidator_PickBoundary_LegalUserTurnOnly(t *testing.T) {
	mem := newTestStore(t)
	provider := &stopOnceProvider{}
	c := NewConsolidator(provider, mem)

	// Each message ~80 chars => ~20 tokens with len(s)/4. The
	// boundary needs to skip the assistant at idx=3 and land on the
	// user at idx=4.
	body := strings.Repeat("a", 80)
	sess := &Session{
		Key:              "cli:direct",
		LastConsolidated: 0,
		Messages: []Message{
			makeTextMsg("user", body),       // 0 - start
			makeTextMsg("assistant", body),  // 1
			makeTextMsg("user", body),       // 2 - candidate boundary
			makeTextMsg("assistant", body),  // 3 - NOT a boundary (assistant)
			makeTextMsg("user", body),       // 4 - second candidate boundary
			makeTextMsg("assistant", body),  // 5
		},
	}

	// 30 tokens worth of removal — must hit idx=2 first then idx=4.
	bnd, ok := c.PickBoundary(sess, 30)
	if !ok {
		t.Fatalf("PickBoundary returned !ok; want a boundary")
	}
	if bnd.Start != 0 {
		t.Errorf("Start = %d, want 0", bnd.Start)
	}
	// We pick the LAST qualifying boundary (idx=4) — this is the
	// upstream-correct behavior because it maximizes how much we
	// drop in this round.
	if bnd.End != 4 {
		t.Errorf("End = %d, want 4 (the second user-message boundary)", bnd.End)
	}

	// Sanity: make sure messages[bnd.End] is actually a user message.
	if sess.Messages[bnd.End].Role != "user" {
		t.Errorf("messages[%d].Role = %q, want 'user'", bnd.End, sess.Messages[bnd.End].Role)
	}
}

// TestConsolidator_NoLegalBoundary_ReturnsFalse — short session where
// no user message past `start` carries enough cumulative tokens to
// satisfy `tokensToRemove`. PickBoundary returns false; Consolidate
// returns ErrNoLegalBoundary.
func TestConsolidator_NoLegalBoundary_ReturnsFalse(t *testing.T) {
	mem := newTestStore(t)
	provider := &stopOnceProvider{}
	c := NewConsolidator(provider, mem)

	// 3 short messages; 999 tokens demanded — no chance.
	sess := &Session{
		Key:              "cli:direct",
		LastConsolidated: 0,
		Messages: []Message{
			makeTextMsg("user", "hi"),
			makeTextMsg("assistant", "hello"),
			makeTextMsg("user", "ok"),
		},
	}

	bnd, ok := c.PickBoundary(sess, 999)
	if ok {
		t.Errorf("PickBoundary ok=true with bnd=%v; want false (insufficient tokens)", bnd)
	}

	// Also verify a session that contains NO subsequent user
	// messages past `start` — even a small target can't pick a
	// boundary because there's nowhere legal to cut.
	noUserSess := &Session{
		Key:              "cli:no-user",
		LastConsolidated: 0,
		Messages: []Message{
			makeTextMsg("user", strings.Repeat("p", 200)),       // start
			makeTextMsg("assistant", strings.Repeat("p", 200)),  // not a boundary
			makeTextMsg("assistant", strings.Repeat("p", 200)),  // not a boundary
		},
	}
	if bnd, ok := c.PickBoundary(noUserSess, 1); ok {
		t.Errorf("PickBoundary ok=true with bnd=%v; want false (no user message follows)", bnd)
	}
	if _, err := c.Consolidate(context.Background(), noUserSess); !errors.Is(err, ErrNoLegalBoundary) {
		t.Errorf("Consolidate err = %v, want ErrNoLegalBoundary", err)
	}
}

// TestConsolidator_Consolidate_PersistsSummary — happy-path test:
// FakeProvider scripts a single LLMResponse with content "summary
// text"; Consolidate should write it via WriteSummary and the file
// should be readable.
func TestConsolidator_Consolidate_PersistsSummary(t *testing.T) {
	mem := newTestStore(t)
	provider := &FakeProvider{Script: []LLMResponse{
		{Content: "summary text", FinishReason: "stop"},
	}}
	c := NewConsolidator(provider, mem)

	body := strings.Repeat("z", 80)
	sess := &Session{
		Key:              "cli:demo",
		LastConsolidated: 0,
		Messages: []Message{
			makeTextMsg("user", body),
			makeTextMsg("assistant", body),
			makeTextMsg("user", body),
			makeTextMsg("assistant", body),
			makeTextMsg("user", body),
		},
	}

	out, err := c.Consolidate(context.Background(), sess)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if out != "summary text" {
		t.Errorf("returned summary = %q, want %q", out, "summary text")
	}
	if provider.Calls() != 1 {
		t.Errorf("provider.Calls = %d, want 1", provider.Calls())
	}

	// Re-read the file via the store so the round-trip is exercised.
	stored, err := mem.ReadSummary("cli:demo")
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if stored != "summary text" {
		t.Errorf("ReadSummary = %q, want %q", stored, "summary text")
	}

	// File should also exist on disk under the sanitized name.
	expected := filepath.Join(mem.Root(), "cli_demo-summary.md")
	if _, err := mem.ReadSummary("cli:demo"); err != nil {
		t.Errorf("expected summary file at %s, ReadSummary failed: %v", expected, err)
	}
}

// TestConsolidator_AdvancesLastConsolidated — same setup as the
// persists-summary test, but assert s.LastConsolidated moved to
// boundary.End (not just incremented).
func TestConsolidator_AdvancesLastConsolidated(t *testing.T) {
	mem := newTestStore(t)
	provider := &FakeProvider{Script: []LLMResponse{
		{Content: "compressed", FinishReason: "stop"},
	}}
	c := NewConsolidator(provider, mem)

	body := strings.Repeat("y", 80)
	sess := &Session{
		Key:              "cli:advance",
		LastConsolidated: 0,
		Messages: []Message{
			makeTextMsg("user", body),
			makeTextMsg("assistant", body),
			makeTextMsg("user", body),
			makeTextMsg("assistant", body),
			makeTextMsg("user", body),
			makeTextMsg("assistant", body),
		},
	}

	// Pre-compute what PickBoundary would pick so the assertion is
	// future-proof against estimator changes: half the tail's tokens.
	preTail := 0
	for _, m := range sess.Messages {
		preTail += c.estimateMessageTokens(m)
	}
	expectedBoundary, ok := c.PickBoundary(sess, int(float64(preTail)*0.5))
	if !ok {
		t.Fatalf("PickBoundary returned !ok in setup")
	}

	if _, err := c.Consolidate(context.Background(), sess); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	if sess.LastConsolidated != expectedBoundary.End {
		t.Errorf("LastConsolidated = %d, want %d", sess.LastConsolidated, expectedBoundary.End)
	}
	if sess.LastConsolidated <= 0 {
		t.Errorf("LastConsolidated = %d, want > 0 (cursor advanced)", sess.LastConsolidated)
	}
}

// TestConsolidator_PickBoundary_SkipsToolResultMessages — extra
// coverage: a user-role message whose only content is `tool_result` is
// NOT a legal cut point. Boundary picker must walk past it.
//
// We don't strictly need this test under the required-tests list, but
// it pins the tool_result invariant — which is the load-bearing
// difference between learn-nanobot's picker and a naive "any user
// message" cut. Cheap to write, very expensive to lose silently.
func TestConsolidator_PickBoundary_SkipsToolResultMessages(t *testing.T) {
	mem := newTestStore(t)
	c := NewConsolidator(&stopOnceProvider{}, mem)

	body := strings.Repeat("q", 80)
	sess := &Session{
		Key:              "cli:tools",
		LastConsolidated: 0,
		Messages: []Message{
			makeTextMsg("user", body),       // 0 - start
			makeTextMsg("assistant", body),  // 1
			makeToolResultMsg("t1", body),   // 2 - NOT a boundary (tool_result)
			makeTextMsg("assistant", body),  // 3
			makeTextMsg("user", body),       // 4 - boundary (real user prompt)
			makeTextMsg("assistant", body),  // 5
		},
	}

	bnd, ok := c.PickBoundary(sess, 30)
	if !ok {
		t.Fatalf("PickBoundary returned !ok; want boundary at idx=4")
	}
	if bnd.End != 4 {
		t.Errorf("End = %d, want 4 (skipped tool_result at idx=2)", bnd.End)
	}
}
