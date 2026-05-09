package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Runner — the inner LLM-↔-tools loop carved out of s03's Loop.
// Mirrors upstream `nanobot/agent/runner.py::AgentRunner`. Compared to
// s03's Loop, the Runner:
//
//   - takes pre-built initial messages instead of constructing
//     `[{role:user, content:prompt}]` itself; this is the seam where s08's
//     ContextBuilder will eventually plug in.
//   - returns a structured AgentRunResult (final content + messages +
//     tools_used + usage + finish_reason), not just a string.
//   - keeps going on tool errors (Registry.Execute already wraps them as
//     "tool error: <msg>"), instead of bailing.
//   - does length-recovery: if the provider returns FinishReason="length"
//     once, the Runner appends a continuation hint and retries. Mirrors
//     `_handle_length_recovery` in nanobot/agent/runner.py.
//
// The Runner deliberately does NOT do: hooks (s09), retries on transient
// provider errors (we still bail), session writes (s06), or context
// assembly (s08). Each of those gets its own chapter.

// AgentRunSpec configures one execution. Mirrors upstream AgentRunSpec.
type AgentRunSpec struct {
	// InitialMessages are the messages already prepared by the caller —
	// typically [{role: user, content: prompt}], but in s08+ this comes
	// from the ContextBuilder (system + memory + skills + history).
	InitialMessages []Message

	// Tools is the registry the Runner asks to execute tool calls.
	Tools *Registry

	// Model is the LLM model id to pass to Provider.Chat.
	Model string

	// MaxIterations caps how many provider calls one Run can make.
	// Upstream defaults to 60; the dossier suggests 20 for teaching.
	MaxIterations int

	// System is the system prompt; threaded through ChatReq.
	System string

	// Verbose toggles per-iteration logging to stdout.
	Verbose bool
}

// AgentRunResult is the structured outcome. Mirrors upstream AgentRunResult.
type AgentRunResult struct {
	// FinalContent is the assistant's last text reply.
	FinalContent string

	// Messages is the full message log including assistant + tool_result
	// blocks. The caller (s05's Bus, s11's Loop) persists these.
	Messages []Message

	// ToolsUsed is the de-duplicated set of tool names actually executed.
	// Useful for telemetry and capture hooks (s09).
	ToolsUsed []string

	// Usage is the running total of token usage across all iterations.
	Usage Usage

	// FinishReason is one of the normalized values: "stop" / "length" /
	// "max_iterations" / "error".
	FinishReason string
}

// Runner carries no per-run state — it's reusable across many Run calls,
// each with its own AgentRunSpec.
type Runner struct {
	Provider Provider
}

func NewRunner(p Provider) *Runner { return &Runner{Provider: p} }

// Run drives the LLM ↔ tools loop until the model stops or we hit
// MaxIterations. The signature is deliberately narrow — no hooks, no
// retries on transient errors, no session writes. Each later chapter wraps
// this with one more concern.
func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
	if spec.MaxIterations <= 0 {
		return AgentRunResult{}, errors.New("MaxIterations must be > 0")
	}
	messages := append([]Message(nil), spec.InitialMessages...)
	usage := Usage{}
	toolsUsed := []string{}
	seenTool := map[string]bool{}

	// Length-recovery state (single shot). Upstream allows up to
	// _MAX_LENGTH_RECOVERIES=3; we keep one to stay focused on the idea.
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

		// Provider error response (rate limit, 5xx, …). The Runner does
		// NOT retry yet — that's a future chapter. We just bail with
		// context, including the retryable hint so the caller can decide.
		if resp.FinishReason == "error" {
			return AgentRunResult{
				Messages:     messages,
				FinishReason: "error",
				Usage:        usage,
			}, fmt.Errorf("iteration %d: provider error %s: %s (retryable=%v)",
				iter, resp.ErrCode, resp.ErrMessage, resp.ErrShouldRetry)
		}

		// Tally token usage across the whole run.
		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens

		// Append the assistant message in its native (Anthropic-shape) form.
		// Protocol requires tool_use blocks to round-trip with their
		// original IDs; the Provider's translator already preserved them.
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
			// Length recovery: append a continuation hint and let the
			// next iteration re-prompt. Mirrors upstream's
			// _handle_length_recovery + build_length_recovery_message().
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

// runToolCalls executes each requested tool and returns the tool_result
// content blocks the assistant message expects. Errors do NOT abort the
// run — Registry.Execute already wraps them as "tool error: <msg>", so
// the LLM sees the failure and can decide what to do.
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
