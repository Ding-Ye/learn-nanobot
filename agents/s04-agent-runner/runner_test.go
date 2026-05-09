package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubTool is a minimal Tool used to exercise the Runner without touching
// the BashTool (which would actually shell out).
type stubTool struct {
	name    string
	out     string
	execErr error
}

func (s *stubTool) Schema() ToolSchema {
	return ToolSchema{Name: s.name, Description: "stub", InputSchema: map[string]interface{}{}}
}

func (s *stubTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	if s.execErr != nil {
		return "", s.execErr
	}
	return s.out, nil
}

func mkInitial(prompt string) []Message {
	return []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: prompt}}}}
}

// TestRunner_StopsOnNoToolCalls — happy path: a single LLMResponse with
// FinishReason="stop" should make the Runner return immediately with the
// assistant text.
func TestRunner_StopsOnNoToolCalls(t *testing.T) {
	prov := &FakeProvider{Script: []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "all set"}},
			Content:         "all set",
			FinishReason:    "stop",
			Usage:           Usage{InputTokens: 10, OutputTokens: 5},
		},
	}}
	r := NewRunner(prov)
	got, err := r.Run(context.Background(), AgentRunSpec{
		InitialMessages: mkInitial("go"),
		Tools:           NewRegistry(),
		MaxIterations:   3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.FinalContent != "all set" {
		t.Fatalf("FinalContent = %q, want %q", got.FinalContent, "all set")
	}
	if got.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", got.FinishReason)
	}
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 5 {
		t.Fatalf("usage not tallied: %+v", got.Usage)
	}
	if len(got.ToolsUsed) != 0 {
		t.Fatalf("ToolsUsed should be empty, got %v", got.ToolsUsed)
	}
	if prov.Calls() != 1 {
		t.Fatalf("provider called %d times, want 1", prov.Calls())
	}
}

// TestRunner_TwoToolCalls_BothExecute — script: tool_calls turn → stop turn.
// Both tool_calls in the first turn should execute (one round-trip), and
// ToolsUsed should reflect both names.
func TestRunner_TwoToolCalls_BothExecute(t *testing.T) {
	prov := &FakeProvider{Script: []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{
				{Type: "tool_use", ID: "id_a", Name: "alpha", Input: map[string]interface{}{"x": 1}},
				{Type: "tool_use", ID: "id_b", Name: "beta", Input: map[string]interface{}{"y": 2}},
			},
			ToolCalls: []ToolCall{
				{ID: "id_a", Name: "alpha", Input: map[string]interface{}{"x": 1}},
				{ID: "id_b", Name: "beta", Input: map[string]interface{}{"y": 2}},
			},
			FinishReason: "tool_calls",
			Usage:        Usage{InputTokens: 20, OutputTokens: 10},
		},
		{
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "done"}},
			Content:         "done",
			FinishReason:    "stop",
			Usage:           Usage{InputTokens: 30, OutputTokens: 4},
		},
	}}
	reg := NewRegistry()
	reg.Register(&stubTool{name: "alpha", out: "alpha-out"})
	reg.Register(&stubTool{name: "beta", out: "beta-out"})

	r := NewRunner(prov)
	got, err := r.Run(context.Background(), AgentRunSpec{
		InitialMessages: mkInitial("run alpha and beta"),
		Tools:           reg,
		MaxIterations:   5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.FinalContent != "done" {
		t.Fatalf("FinalContent = %q, want done", got.FinalContent)
	}
	if len(got.ToolsUsed) != 2 {
		t.Fatalf("ToolsUsed = %v, want [alpha beta]", got.ToolsUsed)
	}
	want := map[string]bool{"alpha": false, "beta": false}
	for _, n := range got.ToolsUsed {
		if _, ok := want[n]; !ok {
			t.Fatalf("unexpected tool name in ToolsUsed: %q", n)
		}
		want[n] = true
	}
	for n, seen := range want {
		if !seen {
			t.Fatalf("expected tool %q to be in ToolsUsed", n)
		}
	}
	// Tally is across both iterations.
	if got.Usage.InputTokens != 50 || got.Usage.OutputTokens != 14 {
		t.Fatalf("usage not summed: %+v", got.Usage)
	}
	// Sanity: messages = initial + assistant(tool_calls) + user(tool_results) + assistant(stop)
	if len(got.Messages) != 4 {
		t.Fatalf("messages len = %d, want 4: %+v", len(got.Messages), got.Messages)
	}
}

// TestRunner_MaxIterations_ErrorsCleanly — the fake keeps returning
// tool_calls forever; the Runner should bail with "max iterations" once
// it hits the cap, and FinishReason should be "max_iterations".
func TestRunner_MaxIterations_ErrorsCleanly(t *testing.T) {
	loop := LLMResponse{
		AssistantBlocks: []ContentBlock{{Type: "tool_use", ID: "id", Name: "alpha",
			Input: map[string]interface{}{}}},
		ToolCalls:    []ToolCall{{ID: "id", Name: "alpha", Input: map[string]interface{}{}}},
		FinishReason: "tool_calls",
	}
	prov := &FakeProvider{Script: []LLMResponse{loop, loop, loop, loop, loop}}
	reg := NewRegistry()
	reg.Register(&stubTool{name: "alpha", out: "spinning"})

	r := NewRunner(prov)
	got, err := r.Run(context.Background(), AgentRunSpec{
		InitialMessages: mkInitial("go forever"),
		Tools:           reg,
		MaxIterations:   3,
	})
	if err == nil || !strings.Contains(err.Error(), "max iterations") {
		t.Fatalf("expected 'max iterations' error, got %v", err)
	}
	if got.FinishReason != "max_iterations" {
		t.Fatalf("FinishReason = %q, want max_iterations", got.FinishReason)
	}
	if prov.Calls() != 3 {
		t.Fatalf("provider called %d times, want 3", prov.Calls())
	}
}

// TestRunner_PartialFailure_ContinuesWithToolError — a tool returns a Go
// error during Execute. Registry.Execute wraps it as "tool error: <msg>",
// so the Runner should NOT abort — it should ship that string back as a
// tool_result content block and let the LLM decide what to do. Then the
// next turn returns "stop" and the run completes successfully.
func TestRunner_PartialFailure_ContinuesWithToolError(t *testing.T) {
	prov := &FakeProvider{Script: []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{{Type: "tool_use", ID: "id_x", Name: "alpha",
				Input: map[string]interface{}{}}},
			ToolCalls:    []ToolCall{{ID: "id_x", Name: "alpha", Input: map[string]interface{}{}}},
			FinishReason: "tool_calls",
		},
		{
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "recovered"}},
			Content:         "recovered",
			FinishReason:    "stop",
		},
	}}
	reg := NewRegistry()
	reg.Register(&stubTool{name: "alpha", execErr: errors.New("disk full")})

	r := NewRunner(prov)
	got, err := r.Run(context.Background(), AgentRunSpec{
		InitialMessages: mkInitial("test"),
		Tools:           reg,
		MaxIterations:   5,
	})
	if err != nil {
		t.Fatalf("Runner should not abort on tool error, got %v", err)
	}
	if got.FinalContent != "recovered" {
		t.Fatalf("FinalContent = %q, want recovered", got.FinalContent)
	}
	// The tool_result block carrying the error string sits in messages[2]
	// (initial=0, assistant tool_calls=1, user tool_results=2, assistant stop=3).
	if len(got.Messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(got.Messages))
	}
	toolResultMsg := got.Messages[2]
	if toolResultMsg.Role != "user" || len(toolResultMsg.Content) != 1 {
		t.Fatalf("unexpected tool_result message: %+v", toolResultMsg)
	}
	body, _ := toolResultMsg.Content[0].ToolContent.(string)
	if !strings.Contains(body, "tool error") {
		t.Fatalf("tool_result body should contain 'tool error', got %q", body)
	}
}

// TestRunner_LengthRecovery_OnePass — script: one length response, one stop
// response. The Runner should append a continuation hint, re-prompt once,
// and succeed. Mirrors upstream's _handle_length_recovery (single shot
// version).
func TestRunner_LengthRecovery_OnePass(t *testing.T) {
	prov := &FakeProvider{Script: []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "partial..."}},
			Content:         "partial...",
			FinishReason:    "length",
		},
		{
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "continuation: ok"}},
			Content:         "continuation: ok",
			FinishReason:    "stop",
		},
	}}
	r := NewRunner(prov)
	got, err := r.Run(context.Background(), AgentRunSpec{
		InitialMessages: mkInitial("write a long answer"),
		Tools:           NewRegistry(),
		MaxIterations:   5,
	})
	if err != nil {
		t.Fatalf("length recovery should succeed once, got %v", err)
	}
	if got.FinalContent != "continuation: ok" {
		t.Fatalf("FinalContent = %q, want continuation: ok", got.FinalContent)
	}
	if got.FinishReason != "stop" {
		t.Fatalf("FinishReason = %q, want stop", got.FinishReason)
	}
	if prov.Calls() != 2 {
		t.Fatalf("provider called %d times, want 2 (length + stop)", prov.Calls())
	}
	// messages = initial + assistant(length) + user(continuation hint) + assistant(stop)
	if len(got.Messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(got.Messages))
	}
	hint, _ := got.Messages[2].Content[0].Text, ""
	if !strings.Contains(hint, "truncated") {
		t.Fatalf("expected truncation hint in messages[2], got %q", hint)
	}
}

// TestRunner_ProviderErrorResponse_Bails — the Provider returns an error
// LLMResponse (FinishReason="error", retryable hint). The Runner does NOT
// retry yet (s09+ adds that); it bails with a wrapped error.
func TestRunner_ProviderErrorResponse_Bails(t *testing.T) {
	prov := &FakeProvider{Script: []LLMResponse{
		{
			FinishReason:   "error",
			ErrCode:        "http_429",
			ErrMessage:     "rate limit",
			ErrShouldRetry: true,
		},
	}}
	r := NewRunner(prov)
	_, err := r.Run(context.Background(), AgentRunSpec{
		InitialMessages: mkInitial("go"),
		Tools:           NewRegistry(),
		MaxIterations:   3,
	})
	if err == nil ||
		!strings.Contains(err.Error(), "rate limit") ||
		!strings.Contains(err.Error(), "retryable=true") {
		t.Fatalf("expected wrapped error with retry hint, got %v", err)
	}
}
