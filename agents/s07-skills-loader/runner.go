package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Runner — the inner LLM-↔-tools loop carved out in s04. Mirrors upstream
// `nanobot/agent/runner.py::AgentRunner`. UNCHANGED in s07 — adding skills
// affects what goes into the system prompt (s08's ContextBuilder territory),
// not what the runner does with messages.

// AgentRunSpec configures one execution. Mirrors upstream AgentRunSpec.
type AgentRunSpec struct {
	InitialMessages []Message
	Tools           *Registry
	Model           string
	MaxIterations   int
	System          string
	Verbose         bool
}

// AgentRunResult is the structured outcome.
type AgentRunResult struct {
	FinalContent string
	Messages     []Message
	ToolsUsed    []string
	Usage        Usage
	FinishReason string
}

// Runner carries no per-run state — it's reusable across many Run calls,
// each with its own AgentRunSpec. Safe to share across goroutines.
type Runner struct {
	Provider Provider
}

func NewRunner(p Provider) *Runner { return &Runner{Provider: p} }

// Run drives the LLM ↔ tools loop until the model stops or we hit MaxIterations.
func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
	if spec.MaxIterations <= 0 {
		return AgentRunResult{}, errors.New("MaxIterations must be > 0")
	}
	messages := append([]Message(nil), spec.InitialMessages...)
	usage := Usage{}
	toolsUsed := []string{}
	seenTool := map[string]bool{}

	lengthRecoveriesLeft := 1

	for iter := 0; iter < spec.MaxIterations; iter++ {
		var tools []ToolSchema
		if spec.Tools != nil {
			tools = spec.Tools.Definitions()
		}

		resp, err := r.Provider.Chat(ctx, ChatReq{
			Messages: messages,
			Tools:    tools,
			Model:    spec.Model,
			System:   spec.System,
		})
		if err != nil {
			return AgentRunResult{
				Messages:     messages,
				FinishReason: "error",
			}, fmt.Errorf("iteration %d: %w", iter, err)
		}

		if resp.FinishReason == "error" {
			return AgentRunResult{
				Messages:     messages,
				FinishReason: "error",
				Usage:        usage,
			}, fmt.Errorf("iteration %d: provider error %s: %s (retryable=%v)",
				iter, resp.ErrCode, resp.ErrMessage, resp.ErrShouldRetry)
		}

		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens

		messages = append(messages, Message{Role: "assistant", Content: resp.AssistantBlocks})

		if spec.Verbose {
			r.dumpAssistant(iter, resp)
		}

		switch resp.FinishReason {
		case "stop":
			return AgentRunResult{
				FinalContent: resp.Content,
				Messages:     messages,
				ToolsUsed:    toolsUsed,
				Usage:        usage,
				FinishReason: "stop",
			}, nil

		case "tool_calls":
			results := r.runToolCalls(ctx, spec.Tools, resp.ToolCalls, iter, spec.Verbose)
			messages = append(messages, Message{Role: "user", Content: results})
			for _, c := range resp.ToolCalls {
				if !seenTool[c.Name] {
					seenTool[c.Name] = true
					toolsUsed = append(toolsUsed, c.Name)
				}
			}

		case "length":
			if lengthRecoveriesLeft <= 0 {
				return AgentRunResult{
					FinalContent: resp.Content,
					Messages:     messages,
					ToolsUsed:    toolsUsed,
					Usage:        usage,
					FinishReason: "length",
				}, fmt.Errorf("iteration %d: response truncated and no length recoveries left", iter)
			}
			lengthRecoveriesLeft--
			if spec.Verbose {
				fmt.Printf("[iter %d] length recovery: re-prompting for continuation\n", iter)
			}
			messages = append(messages, Message{
				Role: "user",
				Content: []ContentBlock{{
					Type: "text",
					Text: "Your previous response was truncated. Please continue and complete it.",
				}},
			})

		default:
			return AgentRunResult{
				Messages:     messages,
				ToolsUsed:    toolsUsed,
				Usage:        usage,
				FinishReason: "error",
			}, fmt.Errorf("iteration %d: unexpected finish_reason %q", iter, resp.FinishReason)
		}
	}

	return AgentRunResult{
		Messages:     messages,
		ToolsUsed:    toolsUsed,
		Usage:        usage,
		FinishReason: "max_iterations",
	}, fmt.Errorf("runner: max iterations (%d) exceeded without stop", spec.MaxIterations)
}

func (r *Runner) runToolCalls(ctx context.Context, registry *Registry, calls []ToolCall, iter int, verbose bool) []ContentBlock {
	var results []ContentBlock
	for _, c := range calls {
		if verbose {
			fmt.Printf("[iter %d] -> %s %v\n", iter, c.Name, c.Input)
		}
		var out string
		if registry == nil {
			out = fmt.Sprintf("tool error: no registry configured for %q", c.Name)
		} else {
			result, err := registry.Execute(ctx, c.Name, c.Input)
			if errors.Is(err, ErrUnknownTool) {
				out = fmt.Sprintf("unknown tool: %q", c.Name)
			} else {
				out = result
			}
		}
		if verbose {
			fmt.Printf("[iter %d] <- %s\n", iter, truncate(out, 240))
		}
		results = append(results, ContentBlock{
			Type:        "tool_result",
			ToolUseID:   c.ID,
			ToolContent: out,
		})
	}
	return results
}

func (r *Runner) dumpAssistant(iter int, resp LLMResponse) {
	if strings.TrimSpace(resp.Content) != "" {
		fmt.Printf("[iter %d] assistant: %s\n", iter, resp.Content)
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
