package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loop_test.go — exercise the outer state machine end to end. We use the
// FakeProvider from s03 (carried over) so tests are deterministic and
// don't touch the network. The MemoryStore is rooted in t.TempDir() so
// every test gets an isolated workspace.
//
// What we DON'T test here:
//
//   - Provider retry semantics (covered in s03).
//   - Tool execution (covered in s02).
//   - Session lock contention (covered in s05).
//   - Skill frontmatter parsing (covered in s07).
//   - Consolidator boundary picker math (covered in s10).
//
// Each prior chapter's tests still hold; s11's tests add the *composition*
// claim: the same components, threaded through the state machine, behave
// the way the state graph predicts.

// newTestLoop constructs a Loop wired to fakes for testing. Returns the
// loop, the underlying FakeProvider (so tests can script Chat
// responses), the MemoryStore root (for inspection), and the
// SDKCaptureHook (for tools_used assertions).
func newTestLoop(t *testing.T, script []LLMResponse) (*Loop, *FakeProvider, *MemoryStore, *SDKCaptureHook) {
	t.Helper()

	root := t.TempDir()
	mem, err := NewMemoryStore(root)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}

	skills := NewSkillLoader("", "", nil)
	cb := NewContextBuilder(mem, skills, nil)

	fp := &FakeProvider{Script: script}
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(fp)

	captureHook := NewSDKCaptureHook()
	runner.SetHook(captureHook)

	sm := NewSessionManager()
	consolidator := NewConsolidator(fp, mem)
	consolidator.Model = "test-model"

	loop := NewLoop(LoopConfig{
		Runner:       runner,
		Tools:        registry,
		CtxBuilder:   cb,
		Sessions:     sm,
		Memory:       mem,
		Consolidator: consolidator,
		Hook:         captureHook,
		Model:        "test-model",
		MaxIter:      4,
	})
	return loop, fp, mem, captureHook
}

// stopReply scripts a single "stop" assistant message — the simplest
// possible inner-loop run.
func stopReply(text string) LLMResponse {
	return LLMResponse{
		Content:         text,
		FinishReason:    "stop",
		AssistantBlocks: []ContentBlock{{Type: "text", Text: text}},
	}
}

// TestLoop_HappyPath_RestoreToDone walks the whole graph for a normal
// user message. Asserts every state was visited in order.
func TestLoop_HappyPath_RestoreToDone(t *testing.T) {
	loop, _, _, _ := newTestLoop(t, []LLMResponse{stopReply("Hello back!")})

	// Wrap dispatch to record the state visited at each tick.
	var visited []TurnState
	original := loopDispatchTrace
	loopDispatchTrace = func(s TurnState) { visited = append(visited, s) }
	defer func() { loopDispatchTrace = original }()

	tc, err := loop.Process(context.Background(), InboundMessage{
		SessionKey: "test:session",
		UserText:   "Hello",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if tc.State != StateDone {
		t.Fatalf("expected StateDone, got %s", tc.State)
	}
	if tc.FinalContent != "Hello back!" {
		t.Fatalf("expected FinalContent='Hello back!', got %q", tc.FinalContent)
	}

	// Expected order: RESTORE, COMPACT, COMMAND, BUILD, RUN, SAVE, RESPOND.
	want := []TurnState{
		StateRestore, StateCompact, StateCommand, StateBuild,
		StateRun, StateSave, StateRespond,
	}
	if len(visited) != len(want) {
		t.Fatalf("visited %d states, expected %d: visited=%v", len(visited), len(want), visited)
	}
	for i, w := range want {
		if visited[i] != w {
			t.Errorf("step %d: got %s, want %s", i, visited[i], w)
		}
	}
}

// TestLoop_CompactState_TriggersConsolidator stuffs a session with enough
// messages to exceed the budget, then asserts the consolidator was
// invoked and a summary was written to disk.
func TestLoop_CompactState_TriggersConsolidator(t *testing.T) {
	// Two scripted Chat responses: one for the consolidator's
	// summarization call, one for the actual user turn.
	loop, fp, mem, _ := newTestLoop(t, []LLMResponse{
		stopReply("compressed summary of older turns"),
		stopReply("Reply to current question."),
	})
	loop.CompactBudget = 50 // generous threshold so we can pad over it

	// Pre-populate the session with messages that push the un-consolidated
	// tail estimate over CompactBudget. Use the same Bus key the test
	// dispatches under.
	sess := loop.Sessions.GetOrCreate("test:long")
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		text := strings.Repeat("padding ", 10) // ~80 chars → 20 tokens estimate
		sess.Append(Message{Role: role, Content: []ContentBlock{{Type: "text", Text: text}}})
	}

	tc, err := loop.Process(context.Background(), InboundMessage{
		SessionKey: "test:long",
		UserText:   "What's up?",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if tc.State != StateDone {
		t.Fatalf("expected StateDone, got %s (err=%v)", tc.State, tc.Err)
	}

	// The Consolidator must have been called once → 2 Chat calls total
	// (consolidator + the user turn). If COMPACT didn't fire, we'd see
	// 1 Chat call (just the user turn).
	if fp.Calls() != 2 {
		t.Fatalf("expected 2 Chat calls (consolidator + run), got %d", fp.Calls())
	}

	// Summary file must have been written by the consolidator.
	summary, err := mem.ReadSummary("test:long")
	if err != nil {
		t.Fatalf("ReadSummary: %v", err)
	}
	if !strings.Contains(summary, "compressed summary") {
		t.Errorf("summary file missing expected text, got %q", summary)
	}
}

// TestLoop_CommandState_SlashHelp_ShortCircuits sends "/help"; asserts
// the LLM is NOT called and FinalContent contains the help text.
func TestLoop_CommandState_SlashHelp_ShortCircuits(t *testing.T) {
	// Empty script: if anything calls the provider, FakeProvider returns
	// "script exhausted" and the test fails.
	loop, fp, _, _ := newTestLoop(t, nil)

	tc, err := loop.Process(context.Background(), InboundMessage{
		SessionKey: "test:cmd",
		UserText:   "/help",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if tc.State != StateDone {
		t.Fatalf("expected StateDone, got %s", tc.State)
	}
	if !strings.Contains(tc.FinalContent, "/help") {
		t.Errorf("FinalContent should contain help text, got %q", tc.FinalContent)
	}
	if fp.Calls() != 0 {
		t.Errorf("expected 0 Chat calls (slash command short-circuited), got %d", fp.Calls())
	}
}

// TestLoop_RunState_ToolUseRoundTrip scripts a tool-call → stop sequence
// inside the inner loop. Asserts that the outer state machine completes
// normally and tools_used is populated.
func TestLoop_RunState_ToolUseRoundTrip(t *testing.T) {
	toolCallResp := LLMResponse{
		FinishReason: "tool_calls",
		ToolCalls: []ToolCall{{
			ID:    "tu_1",
			Name:  "bash",
			Input: map[string]interface{}{"command": "echo hello"},
		}},
		AssistantBlocks: []ContentBlock{{
			Type: "tool_use", ID: "tu_1", Name: "bash",
			Input: map[string]interface{}{"command": "echo hello"},
		}},
	}
	stopResp := stopReply("Tool ran successfully.")

	loop, fp, _, hook := newTestLoop(t, []LLMResponse{toolCallResp, stopResp})

	tc, err := loop.Process(context.Background(), InboundMessage{
		SessionKey: "test:tools",
		UserText:   "Run echo hello",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if tc.State != StateDone {
		t.Fatalf("expected StateDone, got %s (err=%v)", tc.State, tc.Err)
	}
	if fp.Calls() != 2 {
		t.Errorf("expected 2 Chat calls (tool_use + stop), got %d", fp.Calls())
	}
	if len(tc.ToolsUsed) != 1 || tc.ToolsUsed[0] != "bash" {
		t.Errorf("expected ToolsUsed=[bash], got %v", tc.ToolsUsed)
	}
	if got := hook.ToolsUsedSnapshot(); len(got) == 0 {
		t.Errorf("expected hook to capture tool name, got empty slice")
	}
}

// TestLoop_SaveState_PersistsTurn asserts after a happy path the user +
// assistant entries land in history.jsonl.
func TestLoop_SaveState_PersistsTurn(t *testing.T) {
	loop, _, mem, _ := newTestLoop(t, []LLMResponse{stopReply("ack")})

	tc, err := loop.Process(context.Background(), InboundMessage{
		SessionKey: "test:save",
		UserText:   "remember me",
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if tc.State != StateDone {
		t.Fatalf("expected StateDone, got %s", tc.State)
	}

	// history.jsonl should have at least 2 lines (user + assistant).
	body, err := os.ReadFile(filepath.Join(mem.Root(), "history.jsonl"))
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 history lines, got %d: %q", len(lines), string(body))
	}

	// And the in-memory session should have the user + assistant.
	sess, _ := loop.Sessions.Get("test:save")
	if sess == nil {
		t.Fatal("Session disappeared after Process")
	}
	if len(sess.Messages) < 2 {
		t.Errorf("Session.Messages should have user + assistant after Save, got %d", len(sess.Messages))
	}
	hasUser := false
	for _, m := range sess.Messages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		t.Errorf("Session.Messages missing user message; messages=%v", sess.Messages)
	}
}

// TestLoop_ErrorInBuild_TransitionsToError points the MemoryStore at a
// path that doesn't exist after construction (we yank the dir out from
// under it) and asserts the state machine transitions to StateError
// with a non-nil tc.Err. The driver loop must NOT panic.
func TestLoop_ErrorInBuild_TransitionsToError(t *testing.T) {
	loop, _, mem, _ := newTestLoop(t, []LLMResponse{stopReply("never reached")})

	// Yank the memory dir. ContextBuilder.BuildSystemPrompt swallows
	// per-file read errors, but stateBuild's pre-check stat-checks the
	// root and surfaces a "directory missing" error.
	if err := os.RemoveAll(mem.Root()); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	tc, err := loop.Process(context.Background(), InboundMessage{
		SessionKey: "test:err",
		UserText:   "Hi",
	})
	if err == nil {
		t.Fatal("expected error from Process, got nil")
	}
	if tc.State != StateError {
		t.Fatalf("expected StateError, got %s", tc.State)
	}
	if tc.Err == nil {
		t.Fatal("expected tc.Err to be non-nil")
	}
	if !strings.Contains(tc.Err.Error(), "BUILD") {
		t.Errorf("error should be tagged with originating state BUILD, got %q", tc.Err.Error())
	}
}

// TestLoop_ProcessIsIdempotentForSameSessionKey runs two turns under one
// session key; asserts both succeed and the second sees the first's
// history. (Bonus integration check beyond the per-state asserts.)
func TestLoop_ProcessIsIdempotentForSameSessionKey(t *testing.T) {
	loop, _, _, _ := newTestLoop(t, []LLMResponse{
		stopReply("first reply"),
		stopReply("second reply"),
	})

	for i, q := range []string{"first?", "second?"} {
		tc, err := loop.Process(context.Background(), InboundMessage{
			SessionKey: "test:multi",
			UserText:   q,
		})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		if tc.State != StateDone {
			t.Fatalf("turn %d: state=%s err=%v", i, tc.State, tc.Err)
		}
	}

	sess, _ := loop.Sessions.Get("test:multi")
	if sess == nil {
		t.Fatal("Session disappeared")
	}
	// 2 turns → at least 4 messages (user1 + assistant1 + user2 + assistant2).
	if len(sess.Messages) < 4 {
		t.Errorf("expected ≥4 messages after 2 turns, got %d", len(sess.Messages))
	}
}

// TestLoop_NewLoopDefaults asserts NewLoop fills in defaults for unset
// fields and never panics on Process when only required fields are
// provided.
func TestLoop_NewLoopDefaults(t *testing.T) {
	root := t.TempDir()
	mem, _ := NewMemoryStore(root)
	skills := NewSkillLoader("", "", nil)
	cb := NewContextBuilder(mem, skills, nil)
	fp := &FakeProvider{Script: []LLMResponse{stopReply("ok")}}
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(fp)
	sm := NewSessionManager()

	loop := NewLoop(LoopConfig{
		Runner:     runner,
		Tools:      registry,
		CtxBuilder: cb,
		Sessions:   sm,
		Memory:     mem,
	})
	if loop.Hook == nil {
		t.Errorf("Hook should default to NoopHook, got nil")
	}
	if loop.Model == "" {
		t.Errorf("Model should default to claude-sonnet-4-6, got empty")
	}
	if loop.MaxIter <= 0 {
		t.Errorf("MaxIter should default to >0, got %d", loop.MaxIter)
	}
	tc, err := loop.Process(context.Background(), InboundMessage{
		SessionKey: "test:defaults",
		UserText:   "hi",
	})
	if err != nil {
		t.Fatalf("Process under defaults: %v", err)
	}
	if tc.State != StateDone {
		t.Fatalf("expected StateDone, got %s", tc.State)
	}
}

// errorHook is a tiny Hook that always errors on BeforeIter — used by
// downstream chapters but not by s11 tests directly. Kept here so a
// curious reader can see how to fail the inner loop.
type errorHook struct{ NoopHook }

func (errorHook) BeforeIter(ctx context.Context, hc *HookCtx) error {
	return errors.New("test: hook always errors")
}

// loopDispatchTrace lives in loop.go (var declaration there). Tests
// assign / restore it to record visited states.
