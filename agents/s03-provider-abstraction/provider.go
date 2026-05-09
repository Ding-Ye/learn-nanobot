package main

import "context"

// Provider — narrowed and normalized vs. s01/s02. The interface no longer
// returns Anthropic's wire format. It returns a flat LLMResponse that any
// backend (Anthropic, OpenAI-compat, Bedrock, …) can produce.
//
// Phase G adds OpenAI-compat by writing one more impl of this interface;
// the Loop, Registry, etc. don't change.
type Provider interface {
	Chat(ctx context.Context, req ChatReq) (LLMResponse, error)
}

// ChatReq is the provider-agnostic request shape. Each concrete Provider
// translates this into its own wire format internally.
type ChatReq struct {
	Messages    []Message
	Tools       []ToolSchema
	Model       string
	MaxTokens   int
	Temperature float64 // <0 means "use provider default"
	System      string
}

// Message stays the same as s01/s02 — a turn in the conversation.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ContentBlock — same shape as s01/s02. The Provider's translator is what
// reads/writes these; outer code increasingly works with LLMResponse instead.
type ContentBlock struct {
	Type  string                 `json:"type"`
	Text  string                 `json:"text,omitempty"`
	ID    string                 `json:"id,omitempty"`
	Name  string                 `json:"name,omitempty"`
	Input map[string]interface{} `json:"input,omitempty"`

	ToolUseID   string      `json:"tool_use_id,omitempty"`
	ToolContent interface{} `json:"content,omitempty"`
}

type ToolSchema struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// LLMResponse is the normalized output of any Provider.Chat call. Compare
// to upstream nanobot/providers/base.py::LLMResponse — same idea, slimmer.
type LLMResponse struct {
	// Content is the assistant's text reply (concat of all text blocks).
	Content string

	// ToolCalls is the normalized list of tool requests, regardless of
	// whether the wire format used Anthropic-flat blocks or OpenAI-nested
	// `function.arguments` strings.
	ToolCalls []ToolCall

	// AssistantBlocks is the Anthropic-shaped content blocks of the
	// assistant message. The Loop appends this back into history because
	// the protocol requires it. For non-Anthropic providers, the impl
	// synthesizes equivalent blocks.
	AssistantBlocks []ContentBlock

	// FinishReason is normalized to one of:
	//   "stop"        — model decided it's done (Anthropic end_turn / OpenAI stop)
	//   "tool_calls"  — model wants to call tools
	//   "length"      — hit max_tokens; partial response
	//   "error"       — provider returned an error response (see Err* fields)
	FinishReason string

	// Usage is the provider's token-usage report, best-effort normalized.
	Usage Usage

	// Err* fields are populated when FinishReason == "error". They let the
	// caller decide whether to retry. ErrShouldRetry distinguishes
	// transient (rate limit, 5xx) from permanent (quota exhausted).
	ErrCode        string
	ErrMessage     string
	ErrShouldRetry bool
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

// ToolCall is the flat, provider-agnostic tool-request shape.
type ToolCall struct {
	ID    string
	Name  string
	Input map[string]interface{}
}

// HasToolCalls is a small ergonomic helper — Loop reads this rather than
// switching on a string.
func (r LLMResponse) HasToolCalls() bool { return len(r.ToolCalls) > 0 }
