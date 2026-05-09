package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================================
// Round-trip tests for the s03+ Provider shape (LLMResponse, ToolCall).
// We exercise translateRequestToOpenAI / translateResponseFromOpenAI directly
// so no real network is involved.
// ============================================================================

func TestOpenAITranslate_RequestRoundTrip(t *testing.T) {
	req := ChatReq{
		System: "You are helpful.",
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "Hi"}}},
		},
		Tools: []ToolSchema{
			{
				Name:        "bash",
				Description: "Run a shell command",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"command": map[string]interface{}{"type": "string"},
					},
					"required": []string{"command"},
				},
			},
		},
	}
	out := translateRequestToOpenAI(req, "deepseek-chat", 1024)
	if out.Model != "deepseek-chat" || out.MaxTokens != 1024 {
		t.Fatalf("model/max_tokens wrong: %+v", out)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("expected system+user = 2 messages, got %d (%+v)", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content != "You are helpful." {
		t.Fatalf("system message wrong: %+v", out.Messages[0])
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "Hi" {
		t.Fatalf("user message wrong: %+v", out.Messages[1])
	}
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "bash" {
		t.Fatalf("tools translation wrong: %+v", out.Tools)
	}
	props, ok := out.Tools[0].Function.Parameters["properties"].(map[string]interface{})
	if !ok || props["command"] == nil {
		t.Fatalf("parameters lost: %+v", out.Tools[0].Function.Parameters)
	}
}

func TestOpenAITranslate_ResponseRoundTrip(t *testing.T) {
	resp := &openAIChatCompletionResp{
		Choices: []openAIChatChoice{
			{Index: 0, FinishReason: "stop", Message: openAIMessage{Role: "assistant", Content: "Hello!"}},
		},
		Usage: openAIUsage{PromptTokens: 10, CompletionTokens: 5},
	}
	out := translateResponseFromOpenAI(resp)
	if out.FinishReason != "stop" {
		t.Fatalf("finish_reason: %q want stop", out.FinishReason)
	}
	if out.Content != "Hello!" {
		t.Fatalf("content: %q want Hello!", out.Content)
	}
	if len(out.AssistantBlocks) != 1 || out.AssistantBlocks[0].Type != "text" || out.AssistantBlocks[0].Text != "Hello!" {
		t.Fatalf("assistant blocks wrong: %+v", out.AssistantBlocks)
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 {
		t.Fatalf("usage wrong: %+v", out.Usage)
	}
	if out.HasToolCalls() {
		t.Fatalf("HasToolCalls should be false: %+v", out.ToolCalls)
	}

	// length → length
	respLen := &openAIChatCompletionResp{
		Choices: []openAIChatChoice{{FinishReason: "length", Message: openAIMessage{Role: "assistant", Content: "trunc"}}},
	}
	if got := translateResponseFromOpenAI(respLen).FinishReason; got != "length" {
		t.Fatalf("got %q want length", got)
	}
}

func TestOpenAITranslate_ToolUseTrip(t *testing.T) {
	// 1) An assistant tool_use round-trips out to OpenAI tool_calls.
	req := ChatReq{
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: "compute"}}},
			{Role: "assistant", Content: []ContentBlock{
				{Type: "text", Text: "Let me compute."},
				{Type: "tool_use", ID: "toolu_001", Name: "bash", Input: map[string]interface{}{"command": "echo $((2+2))"}},
			}},
			{Role: "user", Content: []ContentBlock{
				{Type: "tool_result", ToolUseID: "toolu_001", ToolContent: "4"},
			}},
		},
	}
	out := translateRequestToOpenAI(req, "x", 0)
	// Expected messages: user("compute") + assistant(with tool_call) + tool("4")
	if len(out.Messages) != 3 {
		t.Fatalf("got %d messages: %+v", len(out.Messages), out.Messages)
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("expected assistant, got %s", asst.Role)
	}
	if asst.Content != "Let me compute." {
		t.Fatalf("text content wrong: %v", asst.Content)
	}
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(asst.ToolCalls))
	}
	tc := asst.ToolCalls[0]
	if tc.ID != "toolu_001" || tc.Function.Name != "bash" {
		t.Fatalf("tool_call wrong: %+v", tc)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not JSON: %q", tc.Function.Arguments)
	}
	if args["command"] != "echo $((2+2))" {
		t.Fatalf("arguments lost: %v", args)
	}
	tool := out.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "toolu_001" || tool.Content != "4" {
		t.Fatalf("tool message wrong: %+v", tool)
	}

	// 2) An OpenAI response with tool_calls round-trips into LLMResponse with
	//    BOTH AssistantBlocks (synthesized) AND a flat ToolCalls slice, plus
	//    finish_reason="tool_calls".
	resp := &openAIChatCompletionResp{
		Choices: []openAIChatChoice{{
			FinishReason: "tool_calls",
			Message: openAIMessage{
				Role:    "assistant",
				Content: "Working on it.",
				ToolCalls: []openAIToolCall{
					{
						ID:   "call_abc123",
						Type: "function",
						Function: openAIToolCallFunc{
							Name:      "bash",
							Arguments: `{"command":"date"}`,
						},
					},
				},
			},
		}},
	}
	rr := translateResponseFromOpenAI(resp)
	if rr.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason: %q want tool_calls", rr.FinishReason)
	}
	if !rr.HasToolCalls() || len(rr.ToolCalls) != 1 {
		t.Fatalf("tool_calls missing: %+v", rr.ToolCalls)
	}
	if rr.ToolCalls[0].Name != "bash" || rr.ToolCalls[0].Input["command"] != "date" {
		t.Fatalf("tool call wrong: %+v", rr.ToolCalls[0])
	}
	// AssistantBlocks: 1 text + 1 tool_use, in order.
	if len(rr.AssistantBlocks) != 2 {
		t.Fatalf("expected 2 assistant blocks, got %d (%+v)", len(rr.AssistantBlocks), rr.AssistantBlocks)
	}
	if rr.AssistantBlocks[0].Type != "text" || rr.AssistantBlocks[0].Text != "Working on it." {
		t.Fatalf("synthesized text block wrong: %+v", rr.AssistantBlocks[0])
	}
	if rr.AssistantBlocks[1].Type != "tool_use" || rr.AssistantBlocks[1].ID != "call_abc123" || rr.AssistantBlocks[1].Name != "bash" {
		t.Fatalf("synthesized tool_use block wrong: %+v", rr.AssistantBlocks[1])
	}

	// 3) Malformed arguments are preserved as _raw_arguments rather than dropped.
	respBad := &openAIChatCompletionResp{
		Choices: []openAIChatChoice{{
			FinishReason: "tool_calls",
			Message: openAIMessage{
				Role: "assistant",
				ToolCalls: []openAIToolCall{
					{ID: "id1", Type: "function", Function: openAIToolCallFunc{Name: "x", Arguments: "not json"}},
				},
			},
		}},
	}
	rb := translateResponseFromOpenAI(respBad)
	if rb.ToolCalls[0].Input["_raw_arguments"] != "not json" {
		t.Fatalf("malformed args not preserved: %+v", rb.ToolCalls[0].Input)
	}
}

// Some OpenAI-compatible servers (notably DeepSeek when content is structured)
// return assistant content as an array of {type:"text", text:"..."} blocks.
// translateResponseFromOpenAI must concatenate the text parts.
func TestOpenAITranslate_ContentArrayShape(t *testing.T) {
	resp := &openAIChatCompletionResp{
		Choices: []openAIChatChoice{{
			FinishReason: "stop",
			Message: openAIMessage{
				Role: "assistant",
				Content: []interface{}{
					map[string]interface{}{"type": "text", "text": "Hello "},
					map[string]interface{}{"type": "text", "text": "world!"},
				},
			},
		}},
	}
	out := translateResponseFromOpenAI(resp)
	if !strings.Contains(out.Content, "Hello world!") {
		t.Fatalf("structured content not concatenated: %+v", out.Content)
	}
	if len(out.AssistantBlocks) != 1 || out.AssistantBlocks[0].Type != "text" {
		t.Fatalf("assistant blocks wrong: %+v", out.AssistantBlocks)
	}
}
