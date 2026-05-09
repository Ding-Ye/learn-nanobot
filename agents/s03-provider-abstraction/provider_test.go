package main

import (
	"context"
	"strings"
	"testing"
)

func TestAnthropic_ClassifyRetry_5xxRetries(t *testing.T) {
	if !classifyRetry(503, []byte("service unavailable")) {
		t.Fatal("503 should be retryable")
	}
	if classifyRetry(400, []byte("bad request")) {
		t.Fatal("400 should not be retryable")
	}
}

func TestAnthropic_ClassifyRetry_429Quota_NoRetry(t *testing.T) {
	body := []byte(`{"error":{"type":"insufficient_quota","message":"You exceeded your quota"}}`)
	if classifyRetry(429, body) {
		t.Fatal("429 with quota marker should not retry")
	}
}

func TestAnthropic_ClassifyRetry_429RateLimit_Retries(t *testing.T) {
	body := []byte(`{"error":{"type":"rate_limit_error","message":"too many requests"}}`)
	if !classifyRetry(429, body) {
		t.Fatal("429 with rate-limit marker should retry")
	}
}

func TestAnthropic_ClassifyRetry_BillingHardLimit_NoRetry(t *testing.T) {
	body := []byte(`{"error":{"type":"billing_hard_limit_reached","message":"hard limit"}}`)
	if classifyRetry(429, body) {
		t.Fatal("billing hard limit should not retry")
	}
}

func TestFakeProvider_DeterministicScript(t *testing.T) {
	f := &FakeProvider{Script: []LLMResponse{
		{Content: "first", FinishReason: "stop"},
		{Content: "second", FinishReason: "stop"},
	}}
	for i, want := range []string{"first", "second"} {
		got, err := f.Chat(context.Background(), ChatReq{})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got.Content != want {
			t.Fatalf("call %d: got %q, want %q", i, got.Content, want)
		}
	}
	// Third call should error.
	_, err := f.Chat(context.Background(), ChatReq{})
	if err == nil || !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("expected exhausted error, got %v", err)
	}
}

func TestLoop_UsesAbstractProvider_HappyPath(t *testing.T) {
	prov := &FakeProvider{Script: []LLMResponse{
		{
			AssistantBlocks: []ContentBlock{{Type: "tool_use", ID: "id_1", Name: "bash",
				Input: map[string]interface{}{"command": "echo"}}},
			ToolCalls:    []ToolCall{{ID: "id_1", Name: "bash", Input: map[string]interface{}{"command": "echo"}}},
			FinishReason: "tool_calls",
		},
		{
			AssistantBlocks: []ContentBlock{{Type: "text", Text: "got it"}},
			Content:         "got it",
			FinishReason:    "stop",
		},
	}}
	r := NewRegistry()
	r.Register(&stubTool{name: "bash", out: "echo output"})

	l := &Loop{Provider: prov, Tools: r, MaxTurns: 5}
	got, err := l.Run(context.Background(), "go")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "got it" {
		t.Fatalf("got %q", got)
	}
}

func TestLoop_ProviderErrorResponse_PropagatesContext(t *testing.T) {
	prov := &FakeProvider{Script: []LLMResponse{
		{
			FinishReason:   "error",
			ErrCode:        "http_429",
			ErrMessage:     "rate limit",
			ErrShouldRetry: true,
		},
	}}
	r := NewRegistry()
	l := &Loop{Provider: prov, Tools: r, MaxTurns: 3}
	_, err := l.Run(context.Background(), "go")
	if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "retryable=true") {
		t.Fatalf("expected wrapped error with retry hint, got %v", err)
	}
}

// stubTool is a minimal Tool for the loop tests above.
type stubTool struct {
	name string
	out  string
}

func (s *stubTool) Schema() ToolSchema {
	return ToolSchema{Name: s.name, Description: "stub", InputSchema: map[string]interface{}{}}
}
func (s *stubTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	return s.out, nil
}
