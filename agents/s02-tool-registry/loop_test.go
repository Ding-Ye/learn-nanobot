package main

import (
	"context"
	"strings"
	"testing"
)

// fakeProvider scripts CreateMessage responses for end-to-end loop tests.
type fakeProvider struct {
	script []*CreateMessageResponse
	calls  int
}

func (f *fakeProvider) CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error) {
	resp := f.script[f.calls]
	f.calls++
	return resp, nil
}

func TestLoop_UsesRegistry_HappyPath(t *testing.T) {
	prov := &fakeProvider{script: []*CreateMessageResponse{
		// turn 0: ask for bash
		{
			Content: []ContentBlock{{
				Type: "tool_use", ID: "id_1", Name: "bash",
				Input: map[string]interface{}{"command": "echo hi"},
			}},
			StopReason: "tool_use",
		},
		// turn 1: end
		{
			Content:    []ContentBlock{{Type: "text", Text: "saw hi"}},
			StopReason: "end_turn",
		},
	}}
	r := NewRegistry()
	r.Register(&stubTool{name: "bash", out: "hi\n"})

	l := &Loop{Provider: prov, Tools: r, MaxTurns: 5}
	got, err := l.Run(context.Background(), "do echo hi")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got, "saw hi") {
		t.Fatalf("got %q", got)
	}
}

func TestLoop_UnknownToolViaRegistry_DoesNotAbort(t *testing.T) {
	prov := &fakeProvider{script: []*CreateMessageResponse{
		// turn 0: ask for ghost
		{
			Content: []ContentBlock{{
				Type: "tool_use", ID: "id_x", Name: "ghost",
				Input: map[string]interface{}{},
			}},
			StopReason: "tool_use",
		},
		// turn 1: model gives up cleanly
		{
			Content:    []ContentBlock{{Type: "text", Text: "couldn't"}},
			StopReason: "end_turn",
		},
	}}
	r := NewRegistry() // empty — every tool call is unknown

	l := &Loop{Provider: prov, Tools: r, MaxTurns: 5}
	got, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "couldn't" {
		t.Fatalf("got %q", got)
	}
	// The unknown-tool path must not abort the loop.
	if prov.calls != 2 {
		t.Fatalf("expected 2 provider calls, got %d", prov.calls)
	}
}
