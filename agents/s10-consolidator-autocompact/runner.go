package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Runner — same shape as s08, but s09 adds the `Hook` field and threads
// hook calls through five well-defined moments in the inner tool-loop.
// Mirrors upstream `nanobot/agent/runner.py::AgentRunner` once the hook
// integration is wired.
//
// Hook call sites (in order, per iteration):
//
//   1. BeforeIter         — fires at the top of the iteration, before the
//                            LLM call. Hooks can stash setup state, log,
//                            or short-circuit by returning a non-nil error
//                            (the run aborts cleanly).
//   2. (LLM call happens; HookCtx.Resp populated)
//   3. BeforeExecuteTools — fires only on iterations where the LLM asks
//                            for tools. Hooks can short-circuit tool exec
//                            by returning a non-nil error (the run aborts
//                            cleanly without invoking any tools this turn).
//   4. (Tools execute; HookCtx.Messages updated with tool_result blocks)
//   5. AfterIter          — fires at the end of every iteration, regardless
//                            of how the iteration ended (stop / tool_calls /
//                            length recovery). Captures the final message
//                            list for SDKCaptureHook.
//
// And once for the whole run:
//
//   6. FinalizeContent    — fires once on the final assistant content,
//                            after the loop ends, before the Runner returns.
//                            Hook return value replaces what callers see.
//
// OnStream is also part of the interface but the Runner doesn't yet
// invoke it — streaming is an extension exercise (see App. B). The
// method exists on the interface so this file's signature doesn't
// have to grow when streaming lands.

// AgentRunSpec — same as s08; carried over verbatim. Hook is *not* on
// the spec because hooks are agent-scoped (one per Runner), not
// per-Run-invocation. If you need per-call dispatch, register a
// CompositeHook and `Add` selectively.
type AgentRunSpec struct {
	InitialMessages []Message
	Tools           *Registry
	Model           string
	MaxIterations   int
	System          string
	Verbose         bool
}

// AgentRunResult — same as s08.
type AgentRunResult struct {
	FinalContent string
	Messages     []Message
	ToolsUsed    []string
	Usage        Usage
	FinishReason string
}

// Runner gains a Hook field in s09. If nil, the Runner uses NoopHook
// internally — saves callers from null checks and keeps the call sites
// uniform. The Hook is reused across every Run call against this
// Runner; it's expected to be agent-scoped.
type Runner struct {
	Provider Provider
	Hook     Hook
}

// NewRunner constructs a Runner with no hook (NoopHook is wired in Run).
// Use SetHook (or set the field directly) to register one.
func NewRunner(p Provider) *Runner { return &Runner{Provider: p} }

// SetHook registers the hook. Call once at construction; the Runner does
// NOT support replacing the hook mid-run.
func (r *Runner) SetHook(h Hook) { r.Hook = h }

// Run drives the LLM ↔ tools loop until the model stops or we hit
// MaxIterations. Hooks fire at five well-defined moments per iteration
// (see the Runner docstring above).
func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
	if spec.MaxIterations <= 0 {
		return AgentRunResult{}, errors.New("MaxIterations must be > 0")
	}

	hook := r.Hook
	if hook == nil {
		hook = NoopHook{}
	}

	messages := append([]Message(nil), spec.InitialMessages...)
	usage := Usage{}
	toolsUsed := []string{}
	seenTool := map[string]bool{}

	lengthRecoveriesLeft := 1

	// HookCtx is shared across all hook calls during this Run. Mutated
	// by the Runner before each call site so the hook sees a consistent
	// view of the iteration in progress.
	hc := &HookCtx{}

	// finalContent is what the loop hands to FinalizeContent at the end.
	// Populated when the loop returns "stop" (or any terminal state with
	// content); zero string otherwise.
	var finalContent string
	var finishReason string

	for iter := 0; iter < spec.MaxIterations; iter++ {
		hc.Iter = iter
		hc.Messages = messages
		hc.Resp = nil
		hc.ToolCalls = nil
		hc.Usage = usage

		// Hook tap point #1: BeforeIter.
		if err := hook.BeforeIter(ctx, hc); err != nil {
			return AgentRunResult{
				Messages:     messages,
				ToolsUsed:    toolsUsed,
				Usage:        usage,
				FinishReason: "error",
			}, fmt.Errorf("iteration %d: hook BeforeIter: %w", iter, err)
		}

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
			hc.Err = err
			// Best-effort AfterIter so observability hooks see the failure.
			_ = hook.AfterIter(ctx, hc)
			return AgentRunResult{
				Messages:     messages,
				FinishReason: "error",
			}, fmt.Errorf("iteration %d: %w", iter, err)
		}

		if resp.FinishReason == "error" {
			hc.Resp = &resp
			hc.Err = fmt.Errorf("provider error %s: %s", resp.ErrCode, resp.ErrMessage)
			_ = hook.AfterIter(ctx, hc)
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

		// HookCtx now reflects the post-LLM-call state of this iteration.
		hc.Resp = &resp
		hc.ToolCalls = resp.ToolCalls
		hc.Messages = messages
		hc.Usage = usage

		if spec.Verbose {
			r.dumpAssistant(iter, resp)
		}

		switch resp.FinishReason {
		case "stop":
			finalContent = resp.Content
			finishReason = "stop"
			// HookCtx already reflects the final state. Fire AfterIter
			// for the iteration that just stopped.
			if err := hook.AfterIter(ctx, hc); err != nil {
				return AgentRunResult{
					FinalContent: finalContent,
					Messages:     messages,
					ToolsUsed:    toolsUsed,
					Usage:        usage,
					FinishReason: finishReason,
				}, fmt.Errorf("iteration %d: hook AfterIter: %w", iter, err)
			}
			finalContent = hook.FinalizeContent(ctx, hc, finalContent)
			return AgentRunResult{
				FinalContent: finalContent,
				Messages:     messages,
				ToolsUsed:    toolsUsed,
				Usage:        usage,
				FinishReason: finishReason,
			}, nil

		case "tool_calls":
			// Hook tap point #3: BeforeExecuteTools. A non-nil error
			// short-circuits the iteration cleanly — no tools run, the
			// run returns with the captured error.
			if err := hook.BeforeExecuteTools(ctx, hc); err != nil {
				_ = hook.AfterIter(ctx, hc)
				return AgentRunResult{
					Messages:     messages,
					ToolsUsed:    toolsUsed,
					Usage:        usage,
					FinishReason: "error",
				}, fmt.Errorf("iteration %d: hook BeforeExecuteTools: %w", iter, err)
			}
			results := r.runToolCalls(ctx, spec.Tools, resp.ToolCalls, iter, spec.Verbose)
			messages = append(messages, Message{Role: "user", Content: results})
			for _, c := range resp.ToolCalls {
				if !seenTool[c.Name] {
					seenTool[c.Name] = true
					toolsUsed = append(toolsUsed, c.Name)
				}
			}
			// Refresh messages snapshot for AfterIter.
			hc.Messages = messages

		case "length":
			if lengthRecoveriesLeft <= 0 {
				finalContent = resp.Content
				finishReason = "length"
				_ = hook.AfterIter(ctx, hc)
				finalContent = hook.FinalizeContent(ctx, hc, finalContent)
				return AgentRunResult{
					FinalContent: finalContent,
					Messages:     messages,
					ToolsUsed:    toolsUsed,
					Usage:        usage,
					FinishReason: finishReason,
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
			hc.Messages = messages

		default:
			finishReason = "error"
			_ = hook.AfterIter(ctx, hc)
			return AgentRunResult{
				Messages:     messages,
				ToolsUsed:    toolsUsed,
				Usage:        usage,
				FinishReason: finishReason,
			}, fmt.Errorf("iteration %d: unexpected finish_reason %q", iter, resp.FinishReason)
		}

		// Hook tap point #5: AfterIter. Fired even on tool_calls /
		// length-recovery iterations because observability hooks want
		// to record the iteration's outcome regardless of whether the
		// loop is about to continue.
		if err := hook.AfterIter(ctx, hc); err != nil {
			return AgentRunResult{
				Messages:     messages,
				ToolsUsed:    toolsUsed,
				Usage:        usage,
				FinishReason: "error",
			}, fmt.Errorf("iteration %d: hook AfterIter: %w", iter, err)
		}
	}

	// Loop fell off the end without hitting `stop` — that's the
	// max_iterations case. We still fire FinalizeContent so the caller
	// sees a consistent shape.
	finishReason = "max_iterations"
	finalContent = hook.FinalizeContent(ctx, hc, finalContent)
	return AgentRunResult{
		FinalContent: finalContent,
		Messages:     messages,
		ToolsUsed:    toolsUsed,
		Usage:        usage,
		FinishReason: finishReason,
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
