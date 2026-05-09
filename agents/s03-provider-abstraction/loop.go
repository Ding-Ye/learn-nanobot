package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Loop — same outer shape as s02, but consumes LLMResponse instead of
// Anthropic's wire format directly. Compare with s02's runTools: we no
// longer iterate ContentBlock by Type. The Provider's translator already
// did that.

type Loop struct {
	Provider Provider
	Tools    *Registry
	MaxTurns int
	Model    string
	System   string
	Verbose  bool
}

func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	messages := []Message{
		{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: userPrompt}},
		},
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		resp, err := l.Provider.Chat(ctx, ChatReq{
			Messages: messages,
			Tools:    l.Tools.Definitions(),
			Model:    l.Model,
			System:   l.System,
		})
		if err != nil {
			return "", fmt.Errorf("turn %d: %w", turn, err)
		}

		// Provider error responses (rate limit, 5xx, …). s04 will retry on
		// transient ones; for now we just bail with context.
		if resp.FinishReason == "error" {
			return "", fmt.Errorf("turn %d: provider error %s: %s (retryable=%v)",
				turn, resp.ErrCode, resp.ErrMessage, resp.ErrShouldRetry)
		}

		// Append the assistant message in its native (Anthropic-shape) form
		// — required by the protocol; tool-use blocks must round-trip.
		messages = append(messages, Message{Role: "assistant", Content: resp.AssistantBlocks})

		if l.Verbose {
			l.dumpAssistant(turn, resp)
		}

		switch resp.FinishReason {
		case "stop":
			return resp.Content, nil

		case "tool_calls":
			results := l.runToolCalls(ctx, resp.ToolCalls, turn)
			messages = append(messages, Message{Role: "user", Content: results})

		case "length":
			return "", fmt.Errorf("hit length limit at turn %d (response was truncated)", turn)

		default:
			return "", fmt.Errorf("unexpected finish_reason %q at turn %d", resp.FinishReason, turn)
		}
	}
	return "", fmt.Errorf("loop exceeded MaxTurns=%d without stop", l.MaxTurns)
}

func (l *Loop) runToolCalls(ctx context.Context, calls []ToolCall, turn int) []ContentBlock {
	var results []ContentBlock
	for _, c := range calls {
		if l.Verbose {
			fmt.Printf("[turn %d] -> %s %v\n", turn, c.Name, c.Input)
		}
		out, err := l.Tools.Execute(ctx, c.Name, c.Input)
		if errors.Is(err, ErrUnknownTool) {
			out = fmt.Sprintf("unknown tool: %q", c.Name)
		}
		if l.Verbose {
			fmt.Printf("[turn %d] <- %s\n", turn, truncate(out, 240))
		}
		results = append(results, ContentBlock{
			Type:        "tool_result",
			ToolUseID:   c.ID,
			ToolContent: out,
		})
	}
	return results
}

func (l *Loop) dumpAssistant(turn int, resp LLMResponse) {
	if strings.TrimSpace(resp.Content) != "" {
		fmt.Printf("[turn %d] assistant: %s\n", turn, resp.Content)
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
