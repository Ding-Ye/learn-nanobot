package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeProvider is a deterministic test double — we hand it a script of
// responses and it returns them in order, one per CreateMessage call.
type fakeProvider struct {
	script []*CreateMessageResponse
	calls  int
	err    error
}

func (f *fakeProvider) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.calls >= len(f.script) {
		return nil, errors.New("fakeProvider: script exhausted")
	}
	resp := f.script[f.calls]
	f.calls++
	return resp, nil
}

// fakeTool returns a fixed string for the bash tool; lets us assert routing
// without spawning real shells.
type fakeTool struct {
	name   string
	schema ToolSchema
	out    string
	err    error
	called int
}

func (t *fakeTool) Schema() ToolSchema { return t.schema }
func (t *fakeTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	t.called++
	return t.out, t.err
}

func TestLoop_NoTools_ReturnsText(t *testing.T) {
	prov := &fakeProvider{script: []*CreateMessageResponse{
		{
			Content:    []ContentBlock{{Type: "text", Text: "hello back"}},
			StopReason: "end_turn",
		},
	}}
	l := &Loop{Provider: prov, Tools: nil, MaxTurns: 5}
	got, err := l.Run(context.Background(), "hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello back" {
		t.Fatalf("got %q, want %q", got, "hello back")
	}
	if prov.calls != 1 {
		t.Fatalf("expected 1 provider call, got %d", prov.calls)
	}
}

func TestLoop_OneToolCall_ExecutesAndContinues(t *testing.T) {
	prov := &fakeProvider{script: []*CreateMessageResponse{
		// turn 0: ask for bash
		{
			Content: []ContentBlock{{
				Type: "tool_use", ID: "call_1", Name: "bash",
				Input: map[string]interface{}{"command": "echo hi"},
			}},
			StopReason: "tool_use",
		},
		// turn 1: model has tool result, replies and stops
		{
			Content:    []ContentBlock{{Type: "text", Text: "the output was hi"}},
			StopReason: "end_turn",
		},
	}}
	tool := &fakeTool{
		name:   "bash",
		schema: ToolSchema{Name: "bash", Description: "fake", InputSchema: map[string]interface{}{}},
		out:    "hi\n",
	}
	l := &Loop{Provider: prov, Tools: []Tool{tool}, MaxTurns: 5}
	got, err := l.Run(context.Background(), "run echo hi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "the output was hi") {
		t.Fatalf("expected final to contain 'the output was hi', got %q", got)
	}
	if tool.called != 1 {
		t.Fatalf("expected 1 tool call, got %d", tool.called)
	}
	if prov.calls != 2 {
		t.Fatalf("expected 2 provider calls, got %d", prov.calls)
	}
}

func TestLoop_UnknownTool_RecordedInToolResult(t *testing.T) {
	prov := &fakeProvider{script: []*CreateMessageResponse{
		// turn 0: ask for a tool the loop doesn't know
		{
			Content: []ContentBlock{{
				Type: "tool_use", ID: "call_x", Name: "ghost",
				Input: map[string]interface{}{},
			}},
			StopReason: "tool_use",
		},
		// turn 1: model gives up
		{
			Content:    []ContentBlock{{Type: "text", Text: "couldn't"}},
			StopReason: "end_turn",
		},
	}}
	l := &Loop{Provider: prov, Tools: nil, MaxTurns: 5}
	got, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "couldn't" {
		t.Fatalf("got %q, want %q", got, "couldn't")
	}
	// The unknown-tool branch should NOT abort the loop; it should produce a
	// tool_result with the error string and let the model continue.
	if prov.calls != 2 {
		t.Fatalf("expected 2 provider calls (loop must continue past unknown tool), got %d", prov.calls)
	}
}

func TestLoop_MaxTurnsExceeded_ReturnsError(t *testing.T) {
	// Provider scripts an infinite tool_use — loop must bail at MaxTurns.
	endless := &CreateMessageResponse{
		Content: []ContentBlock{{
			Type: "tool_use", ID: "call_n", Name: "bash",
			Input: map[string]interface{}{"command": ":"},
		}},
		StopReason: "tool_use",
	}
	prov := &fakeProvider{script: []*CreateMessageResponse{endless, endless, endless, endless}}
	tool := &fakeTool{
		name:   "bash",
		schema: ToolSchema{Name: "bash"},
		out:    "ok",
	}
	l := &Loop{Provider: prov, Tools: []Tool{tool}, MaxTurns: 3}
	_, err := l.Run(context.Background(), "loop forever")
	if err == nil {
		t.Fatalf("expected an error after MaxTurns=3, got nil")
	}
	if !strings.Contains(err.Error(), "MaxTurns") {
		t.Fatalf("expected MaxTurns in error, got %v", err)
	}
}

func TestLoop_MaxTokens_ReturnsError(t *testing.T) {
	prov := &fakeProvider{script: []*CreateMessageResponse{
		{
			Content:    []ContentBlock{{Type: "text", Text: "truncat..."}},
			StopReason: "max_tokens",
		},
	}}
	l := &Loop{Provider: prov, MaxTurns: 2}
	_, err := l.Run(context.Background(), "long")
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("expected max_tokens error, got %v", err)
	}
}

func TestExtractText_Concatenates(t *testing.T) {
	got := extractText([]ContentBlock{
		{Type: "text", Text: "hello "},
		{Type: "tool_use", Name: "bash"},
		{Type: "text", Text: "world"},
	})
	if got != "hello world" {
		t.Fatalf("got %q", got)
	}
}
