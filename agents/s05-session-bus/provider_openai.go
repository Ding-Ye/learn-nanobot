package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIProvider is the second concrete impl of the s03 Provider interface
// (after AnthropicProvider). It targets ANY OpenAI-compatible Chat
// Completions endpoint: OpenAI itself, DeepSeek, Moonshot/Kimi, Qwen via
// DashScope's compat endpoint, Together AI, Groq, OpenRouter, self-hosted
// vLLM / SGLang, etc.
//
// The Loop never sees the OpenAI wire format. This file does the full
// translation in two directions:
//
//	ChatReq           → OpenAI Chat Completions request
//	OpenAI response   → LLMResponse (with synthesized AssistantBlocks)
//
// Why synthesize AssistantBlocks? Because the Runner appends them back
// into history (the Anthropic protocol shape is what we keep internally).
// Tool calls returned as `tool_calls` are mapped back to `tool_use`
// blocks so the next turn's user-side `tool_result` blocks line up by
// `tool_use_id`. This is the same trick upstream nanobot uses in its
// OpenAIProvider.
type OpenAIProvider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// NewOpenAIProvider builds a provider against the given base URL.
// Common values:
//
//	https://api.openai.com/v1
//	https://api.deepseek.com/v1
//	https://api.moonshot.cn/v1                                (Moonshot/Kimi)
//	https://dashscope.aliyuncs.com/compatible-mode/v1         (Qwen / 通义千问)
//	https://api.groq.com/openai/v1                            (Groq)
//	https://openrouter.ai/api/v1                              (OpenRouter)
//	http://localhost:8000/v1                                  (self-hosted vLLM/SGLang)
func NewOpenAIProvider(apiKey, baseURL, model string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (o *OpenAIProvider) Chat(ctx context.Context, req ChatReq) (LLMResponse, error) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	oaiReq := translateRequestToOpenAI(req, model, maxTokens)
	body, err := json.Marshal(oaiReq)
	if err != nil {
		return LLMResponse{}, fmt.Errorf("encode openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return LLMResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		return LLMResponse{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return LLMResponse{}, err
	}

	// Non-2xx: classify into retry vs. permanent so the Runner can react.
	// Same shape as AnthropicProvider's error path.
	if resp.StatusCode/100 != 2 {
		return LLMResponse{
			FinishReason:   "error",
			ErrCode:        fmt.Sprintf("http_%d", resp.StatusCode),
			ErrMessage:     truncateBody(string(respBody)),
			ErrShouldRetry: classifyRetry(resp.StatusCode, respBody),
		}, nil
	}

	var oaiResp openAIChatCompletionResp
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return LLMResponse{}, fmt.Errorf("decode openai response: %w (body=%s)", err, truncateBody(string(respBody)))
	}
	return translateResponseFromOpenAI(&oaiResp), nil
}

// ---------------------------------------------------------------- wire types

type openAIChatRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`              // "system" | "user" | "assistant" | "tool"
	Content    interface{}      `json:"content,omitempty"` // string OR null when tool_calls is present
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // only for role="tool"
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // "function"
	Function openAIToolCallFunc `json:"function"`
}

type openAIToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string
}

type openAITool struct {
	Type     string        `json:"type"` // "function"
	Function openAIToolDef `json:"function"`
}

type openAIToolDef struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type openAIChatCompletionResp struct {
	Choices []openAIChatChoice `json:"choices"`
	Usage   openAIUsage        `json:"usage"`
}

type openAIChatChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// --------------------------------------------------- translation: out (req)

// translateRequestToOpenAI converts a ChatReq into an OpenAI Chat Completions
// request, in three passes:
//
//  1. system prompt becomes a {role:"system"} message
//  2. each ChatReq.Messages item expands into 1+ OpenAI messages; assistant
//     tool_use blocks become tool_calls on the assistant message; user
//     tool_result blocks become separate {role:"tool"} messages
//  3. tool definitions get wrapped under {type:"function", function:{...}}
func translateRequestToOpenAI(req ChatReq, model string, maxTokens int) openAIChatRequest {
	out := openAIChatRequest{
		Model:     model,
		MaxTokens: maxTokens,
	}
	if req.System != "" {
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, anthropicMessageToOpenAI(m)...)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, openAITool{
			Type: "function",
			Function: openAIToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

// anthropicMessageToOpenAI handles the four cases:
//
//	user   + text blocks      → one user message with concatenated text
//	user   + tool_result      → one tool message PER tool_result block
//	assistant + text          → one assistant message with content
//	assistant + tool_use      → one assistant message with tool_calls
//
// A single user message can contain BOTH text and tool_result, in which
// case we emit a user message followed by N tool messages.
func anthropicMessageToOpenAI(m Message) []openAIMessage {
	var out []openAIMessage
	switch m.Role {
	case "user":
		var texts []string
		var tools []openAIMessage
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "tool_result":
				content := stringifyToolResult(b.ToolContent)
				tools = append(tools, openAIMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    content,
				})
			}
		}
		if len(texts) > 0 {
			out = append(out, openAIMessage{Role: "user", Content: strings.Join(texts, "\n")})
		}
		out = append(out, tools...)

	case "assistant":
		var texts []string
		var calls []openAIToolCall
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if b.Text != "" {
					texts = append(texts, b.Text)
				}
			case "tool_use":
				args, _ := json.Marshal(b.Input)
				calls = append(calls, openAIToolCall{
					ID:   b.ID,
					Type: "function",
					Function: openAIToolCallFunc{
						Name:      b.Name,
						Arguments: string(args),
					},
				})
			}
		}
		msg := openAIMessage{Role: "assistant"}
		if len(texts) > 0 {
			msg.Content = strings.Join(texts, "\n")
		}
		if len(calls) > 0 {
			msg.ToolCalls = calls
		}
		out = append(out, msg)

	case "system":
		// Rare — ChatReq.System is normally where it lives. Pass through.
		var texts []string
		for _, b := range m.Content {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
		if len(texts) > 0 {
			out = append(out, openAIMessage{Role: "system", Content: strings.Join(texts, "\n")})
		}
	}
	return out
}

func stringifyToolResult(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	if data, err := json.Marshal(v); err == nil {
		return string(data)
	}
	return fmt.Sprintf("%v", v)
}

// --------------------------------------------------- translation: in (resp)

// translateResponseFromOpenAI converts an OpenAI Chat Completions response
// into LLMResponse (s03+ shape).
//
// finish_reason mapping (s03+ vocabulary):
//
//	"stop"                    → "stop"
//	"tool_calls" / "function_call" → "tool_calls"
//	"length"                  → "length"
//	other                     → "stop" (best-effort)
//
// AssistantBlocks is synthesized from the assistant message: text blocks
// for any text content, tool_use blocks for each tool call. The Runner
// appends these to the message history so subsequent turns can reference
// them by ID. ToolCalls is populated separately as the flat list the
// Runner reads via HasToolCalls().
func translateResponseFromOpenAI(resp *openAIChatCompletionResp) LLMResponse {
	out := LLMResponse{
		Usage: Usage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		},
	}
	if len(resp.Choices) == 0 {
		out.FinishReason = "stop"
		return out
	}
	choice := resp.Choices[0]

	// Text content — strings or {type:text,text:...} arrays (DeepSeek etc.).
	if text, ok := contentToString(choice.Message.Content); ok && strings.TrimSpace(text) != "" {
		out.Content = text
		out.AssistantBlocks = append(out.AssistantBlocks, ContentBlock{Type: "text", Text: text})
	}

	// Tool calls — synthesize tool_use blocks AND populate ToolCalls.
	for _, tc := range choice.Message.ToolCalls {
		var input map[string]interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
			input = map[string]interface{}{"_raw_arguments": tc.Function.Arguments}
		}
		out.AssistantBlocks = append(out.AssistantBlocks, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	switch choice.FinishReason {
	case "stop", "":
		out.FinishReason = "stop"
	case "tool_calls", "function_call":
		out.FinishReason = "tool_calls"
	case "length":
		out.FinishReason = "length"
	default:
		out.FinishReason = "stop"
	}
	return out
}

func contentToString(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case nil:
		return "", false
	case []interface{}:
		// Some providers (notably DeepSeek) return content as an array
		// of {type:"text", text:"..."} blocks. Concatenate the text parts.
		var parts []string
		for _, item := range x {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if txt, ok := m["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		return strings.Join(parts, ""), len(parts) > 0
	}
	return "", false
}
