package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// recordingHook is a Hook that appends the names of called methods to a
// slice for assertion. Each call record is a string like "BeforeIter(0)"
// or "AfterIter(1)".
type recordingHook struct {
	mu    sync.Mutex
	calls []string
}

func (h *recordingHook) record(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, s)
}

func (h *recordingHook) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func (h *recordingHook) BeforeIter(ctx context.Context, hc *HookCtx) error {
	h.record(formatCall("BeforeIter", hc.Iter))
	return nil
}
func (h *recordingHook) OnStream(ctx context.Context, hc *HookCtx, delta string) error {
	h.record(formatCall("OnStream", hc.Iter))
	return nil
}
func (h *recordingHook) BeforeExecuteTools(ctx context.Context, hc *HookCtx) error {
	h.record(formatCall("BeforeExecuteTools", hc.Iter))
	return nil
}
func (h *recordingHook) AfterIter(ctx context.Context, hc *HookCtx) error {
	h.record(formatCall("AfterIter", hc.Iter))
	return nil
}
func (h *recordingHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
	h.record("FinalizeContent")
	return content
}

func formatCall(name string, iter int) string {
	return name + "(" + itoa(iter) + ")"
}

// itoa avoids the strconv import for this small helper.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// makeTwoTurnScript returns a 2-iteration script: turn 1 requests one
// tool, turn 2 stops cleanly.
func makeTwoTurnScript() []LLMResponse {
	return []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{{Type: "tool_use", ID: "t1", Name: "bash", Input: map[string]interface{}{"command": "echo hi"}}},
			ToolCalls: []ToolCall{
				{ID: "t1", Name: "bash", Input: map[string]interface{}{"command": "echo hi"}},
			},
			FinishReason: "tool_calls",
			Usage:        Usage{InputTokens: 10, OutputTokens: 5},
		},
		{
			Content:         "all done",
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "all done"}},
			FinishReason:    "stop",
			Usage:           Usage{InputTokens: 12, OutputTokens: 4},
		},
	}
}

// TestHook_BeforeIter_AfterIter_CalledInOrder — register a recording
// hook; run a 2-turn loop; assert the call order matches the contract:
// BeforeIter(0), BeforeExecuteTools(0), AfterIter(0), BeforeIter(1),
// AfterIter(1), FinalizeContent.
func TestHook_BeforeIter_AfterIter_CalledInOrder(t *testing.T) {
	provider := &FakeProvider{Script: makeTwoTurnScript()}
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(provider)
	hook := &recordingHook{}
	runner.SetHook(hook)

	_, err := runner.Run(context.Background(), AgentRunSpec{
		InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:           registry,
		Model:           "test",
		MaxIterations:   5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := hook.Calls()
	want := []string{
		"BeforeIter(0)",
		"BeforeExecuteTools(0)",
		"AfterIter(0)",
		"BeforeIter(1)",
		"AfterIter(1)",
		"FinalizeContent",
	}
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// panicHook panics in BeforeIter to test error isolation.
type panicHook struct{}

func (panicHook) BeforeIter(ctx context.Context, hc *HookCtx) error {
	panic("intentional panic from panicHook")
}
func (panicHook) OnStream(ctx context.Context, hc *HookCtx, delta string) error { return nil }
func (panicHook) BeforeExecuteTools(ctx context.Context, hc *HookCtx) error     { return nil }
func (panicHook) AfterIter(ctx context.Context, hc *HookCtx) error              { return nil }
func (panicHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
	return content
}

// TestCompositeHook_OneFails_OthersStillRun — register hook A (panics
// in BeforeIter) and hook B (records); assert B's BeforeIter still runs
// even though A's panicked.
func TestCompositeHook_OneFails_OthersStillRun(t *testing.T) {
	hookA := panicHook{}
	hookB := &recordingHook{}

	composite := NewCompositeHook(hookA, hookB)
	// Suppress the default panic log so the test output stays clean.
	composite.LogPanic = func(method string, h Hook, recovered any) {}

	provider := &FakeProvider{Script: []LLMResponse{
		{
			Content:         "done",
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "done"}},
			FinishReason:    "stop",
		},
	}}
	runner := NewRunner(provider)
	runner.SetHook(composite)

	// The composite returns an error from BeforeIter (because hook A
	// panicked) but hook B *still ran* — that's the invariant we're
	// testing.
	_, err := runner.Run(context.Background(), AgentRunSpec{
		InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Model:           "test",
		MaxIterations:   3,
	})
	if err == nil {
		t.Fatalf("expected Run to surface the BeforeIter panic as an error")
	}

	calls := hookB.Calls()
	if len(calls) == 0 {
		t.Fatalf("hook B never ran — composite did not isolate hook A's panic")
	}
	if calls[0] != "BeforeIter(0)" {
		t.Errorf("hook B's first call = %q, want BeforeIter(0)", calls[0])
	}
}

// TestSDKCaptureHook_AccumulatesToolsUsed — run a script with two
// tool_calls (across two iterations) and assert ToolsUsed has both names.
func TestSDKCaptureHook_AccumulatesToolsUsed(t *testing.T) {
	provider := &FakeProvider{Script: []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{{Type: "tool_use", ID: "t1", Name: "bash", Input: map[string]interface{}{"command": "echo a"}}},
			ToolCalls: []ToolCall{
				{ID: "t1", Name: "bash", Input: map[string]interface{}{"command": "echo a"}},
			},
			FinishReason: "tool_calls",
		},
		{
			AssistantBlocks: []ContentBlock{{Type: "tool_use", ID: "t2", Name: "bash", Input: map[string]interface{}{"command": "echo b"}}},
			ToolCalls: []ToolCall{
				{ID: "t2", Name: "bash", Input: map[string]interface{}{"command": "echo b"}},
			},
			FinishReason: "tool_calls",
		},
		{
			Content:         "done",
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "done"}},
			FinishReason:    "stop",
		},
	}}
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(provider)
	cap := NewSDKCaptureHook()
	runner.SetHook(cap)

	_, err := runner.Run(context.Background(), AgentRunSpec{
		InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:           registry,
		Model:           "test",
		MaxIterations:   5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	tools := cap.ToolsUsedSnapshot()
	if len(tools) != 2 {
		t.Fatalf("ToolsUsed = %v, want 2 entries", tools)
	}
	for _, n := range tools {
		if n != "bash" {
			t.Errorf("expected all entries to be 'bash', got %q", n)
		}
	}

	// FinalMessages should be a snapshot of the last AfterIter's
	// messages slice — non-empty after a Run that produced any output.
	finals := cap.FinalMessagesSnapshot()
	if len(finals) == 0 {
		t.Errorf("FinalMessages snapshot is empty; expected the end-of-turn message list")
	}
}

// rewriteHook prepends a fixed string to FinalizeContent's input.
type rewriteHook struct {
	NoopHook
	Prefix string
}

func (r *rewriteHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
	return r.Prefix + content
}

// TestHook_FinalizeContent_CanRewrite — register a hook whose
// FinalizeContent returns "rewritten:" + content; assert the Runner's
// FinalContent has the prefix.
func TestHook_FinalizeContent_CanRewrite(t *testing.T) {
	provider := &FakeProvider{Script: []LLMResponse{
		{
			Content:         "hello",
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "hello"}},
			FinishReason:    "stop",
		},
	}}
	runner := NewRunner(provider)
	runner.SetHook(&rewriteHook{Prefix: "rewritten:"})

	result, err := runner.Run(context.Background(), AgentRunSpec{
		InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Model:           "test",
		MaxIterations:   2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(result.FinalContent, "rewritten:") {
		t.Errorf("FinalContent = %q, want prefix 'rewritten:'", result.FinalContent)
	}
}

// shortcircuitHook returns a sentinel error from BeforeExecuteTools.
type shortcircuitHook struct {
	NoopHook
	Sentinel error
	Calls    []string
	mu       sync.Mutex
}

func (s *shortcircuitHook) BeforeExecuteTools(ctx context.Context, hc *HookCtx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, "BeforeExecuteTools")
	return s.Sentinel
}

func (s *shortcircuitHook) AfterIter(ctx context.Context, hc *HookCtx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, "AfterIter")
	return nil
}

// TestHook_BeforeExecuteTools_CanShortcircuit — return a sentinel error
// from BeforeExecuteTools; the runner should bail mid-iteration cleanly
// (no tools run, the error is surfaced to the caller).
func TestHook_BeforeExecuteTools_CanShortcircuit(t *testing.T) {
	sentinel := errors.New("user cancelled before tool exec")

	// Track whether the bash tool was actually invoked.
	var toolRan bool
	var toolMu sync.Mutex
	tool := &countedTool{
		schema: ToolSchema{Name: "bash", InputSchema: map[string]interface{}{"type": "object"}},
		exec: func() {
			toolMu.Lock()
			toolRan = true
			toolMu.Unlock()
		},
	}
	registry := NewRegistry()
	registry.Register(tool)

	provider := &FakeProvider{Script: []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{{Type: "tool_use", ID: "t1", Name: "bash", Input: map[string]interface{}{"command": "echo hi"}}},
			ToolCalls: []ToolCall{
				{ID: "t1", Name: "bash", Input: map[string]interface{}{"command": "echo hi"}},
			},
			FinishReason: "tool_calls",
		},
	}}
	runner := NewRunner(provider)
	hook := &shortcircuitHook{Sentinel: sentinel}
	runner.SetHook(hook)

	_, err := runner.Run(context.Background(), AgentRunSpec{
		InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools:           registry,
		Model:           "test",
		MaxIterations:   5,
	})
	if err == nil {
		t.Fatalf("expected Run to surface BeforeExecuteTools sentinel error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Run error = %v, want chain to wrap sentinel", err)
	}

	toolMu.Lock()
	defer toolMu.Unlock()
	if toolRan {
		t.Errorf("tool ran despite BeforeExecuteTools short-circuit")
	}
	// Sanity check: AfterIter still fired so observability is preserved.
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if len(hook.Calls) < 2 || hook.Calls[0] != "BeforeExecuteTools" || hook.Calls[1] != "AfterIter" {
		t.Errorf("hook calls = %v, want [BeforeExecuteTools AfterIter]", hook.Calls)
	}
}

// countedTool is a tiny test tool that runs an arbitrary callback when
// Execute is invoked. We use it to detect whether the runner actually
// ran the tool body.
type countedTool struct {
	schema ToolSchema
	exec   func()
}

func (c *countedTool) Schema() ToolSchema { return c.schema }
func (c *countedTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	if c.exec != nil {
		c.exec()
	}
	return "ok", nil
}

// TestRunner_NoHook_DefaultsToNoop — the Runner should run cleanly
// without any hook configured.
func TestRunner_NoHook_DefaultsToNoop(t *testing.T) {
	provider := &FakeProvider{Script: []LLMResponse{
		{
			Content:         "ok",
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "ok"}},
			FinishReason:    "stop",
		},
	}}
	runner := NewRunner(provider) // no SetHook

	result, err := runner.Run(context.Background(), AgentRunSpec{
		InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Model:           "test",
		MaxIterations:   2,
	})
	if err != nil {
		t.Fatalf("Run with no hook: %v", err)
	}
	if result.FinalContent != "ok" {
		t.Errorf("FinalContent = %q, want %q", result.FinalContent, "ok")
	}
}
