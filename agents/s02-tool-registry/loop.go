package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Loop is the same outer shape as s01. The diff with s01:
//   - Tools field is now *Registry (was []Tool)
//   - tool dispatch goes through Registry.Get / Registry.Execute
//   - schema export goes through Registry.Definitions (cached)
type Loop struct {
	Provider Provider
	Tools    *Registry
	MaxTurns int
	Verbose  bool
}

func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
	schemas := l.Tools.Definitions()

	messages := []Message{
		{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: userPrompt}},
		},
	}

	for turn := 0; turn < l.MaxTurns; turn++ {
		resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
			Messages: messages,
			Tools:    schemas,
		})
		if err != nil {
			return "", fmt.Errorf("turn %d: %w", turn, err)
		}

		messages = append(messages, Message{Role: "assistant", Content: resp.Content})

		if l.Verbose {
			l.dumpAssistant(turn, resp)
		}

		switch resp.StopReason {
		case "end_turn", "stop_sequence":
			return extractText(resp.Content), nil

		case "tool_use":
			results := l.runTools(ctx, resp.Content, turn)
			messages = append(messages, Message{Role: "user", Content: results})

		case "max_tokens":
			return "", fmt.Errorf("hit max_tokens at turn %d (response was truncated)", turn)

		default:
			return "", fmt.Errorf("unexpected stop_reason %q at turn %d", resp.StopReason, turn)
		}
	}
	return "", fmt.Errorf("loop exceeded MaxTurns=%d without end_turn", l.MaxTurns)
}

func (l *Loop) runTools(ctx context.Context, content []ContentBlock, turn int) []ContentBlock {
	var results []ContentBlock
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}
		if l.Verbose {
			fmt.Printf("[turn %d] -> %s %v\n", turn, block.Name, block.Input)
		}
		out, err := l.Tools.Execute(ctx, block.Name, block.Input)
		// ErrUnknownTool is the only error Execute returns directly; tool
		// runtime errors are stringified into `out`. Either way, we always
		// produce a tool_result so the loop can continue.
		if errors.Is(err, ErrUnknownTool) {
			out = fmt.Sprintf("unknown tool: %q", block.Name)
		}
		if l.Verbose {
			fmt.Printf("[turn %d] <- %s\n", turn, truncate(out, 240))
		}
		results = append(results, ContentBlock{
			Type:        "tool_result",
			ToolUseID:   block.ID,
			ToolContent: out,
		})
	}
	return results
}

func (l *Loop) dumpAssistant(turn int, resp *CreateMessageResponse) {
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			fmt.Printf("[turn %d] assistant: %s\n", turn, b.Text)
		}
	}
}

func extractText(content []ContentBlock) string {
	var sb strings.Builder
	for _, b := range content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
