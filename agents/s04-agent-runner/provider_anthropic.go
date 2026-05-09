package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AnthropicProvider implements Provider using Anthropic's /v1/messages.
// Same as s03; carried over because each session is its own Go module.

type AnthropicProvider struct {
	apiKey string
	model  string
	client *http.Client
}

func NewAnthropicProvider(apiKey, model string) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 120 * time.Second},
	}
}

type anthropicMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

type anthropicReq struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []ToolSchema       `json:"tools,omitempty"`
	System    string             `json:"system,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResp struct {
	ID         string         `json:"id"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      anthropicUsage `json:"usage"`
}

func (a *AnthropicProvider) Chat(ctx context.Context, req ChatReq) (LLMResponse, error) {
	model := req.Model
	if model == "" {
		model = a.model
	}
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = 4096
	}

	wreq := anthropicReq{
		Model:     model,
		MaxTokens: maxTok,
		System:    req.System,
		Tools:     req.Tools,
	}
	for _, m := range req.Messages {
		wreq.Messages = append(wreq.Messages, anthropicMessage{Role: m.Role, Content: m.Content})
	}

	body, err := json.Marshal(wreq)
	if err != nil {
		return LLMResponse{}, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return LLMResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return LLMResponse{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return LLMResponse{}, err
	}

	if resp.StatusCode/100 != 2 {
		return LLMResponse{
			FinishReason:   "error",
			ErrCode:        fmt.Sprintf("http_%d", resp.StatusCode),
			ErrMessage:     truncateBody(string(respBody)),
			ErrShouldRetry: classifyRetry(resp.StatusCode, respBody),
		}, nil
	}

	var aresp anthropicResp
	if err := json.Unmarshal(respBody, &aresp); err != nil {
		return LLMResponse{}, fmt.Errorf("decode response: %w (body=%s)", err, truncateBody(string(respBody)))
	}

	out := LLMResponse{
		AssistantBlocks: aresp.Content,
		Usage:           Usage{InputTokens: aresp.Usage.InputTokens, OutputTokens: aresp.Usage.OutputTokens},
	}
	for _, b := range aresp.Content {
		switch b.Type {
		case "text":
			out.Content += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input,
			})
		}
	}
	switch aresp.StopReason {
	case "end_turn", "stop_sequence":
		out.FinishReason = "stop"
	case "tool_use":
		out.FinishReason = "tool_calls"
	case "max_tokens":
		out.FinishReason = "length"
	default:
		out.FinishReason = aresp.StopReason
	}
	return out, nil
}

// classifyRetry implements a slim version of upstream's heuristic: 429 with
// quota markers → no retry; 429 with rate-limit markers → retry; 5xx →
// retry; everything else → no retry.
func classifyRetry(status int, body []byte) bool {
	if status >= 500 && status < 600 {
		return true
	}
	if status == 429 {
		s := string(body)
		for _, marker := range []string{"insufficient_quota", "quota_exceeded", "billing_hard_limit", "insufficient_balance"} {
			if containsCI(s, marker) {
				return false
			}
		}
		return true
	}
	return false
}

func containsCI(s, sub string) bool {
	if len(sub) == 0 || len(s) == 0 {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func truncateBody(s string) string {
	const cap = 500
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "…"
}
