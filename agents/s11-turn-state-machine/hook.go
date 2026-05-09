package main

import (
	"context"
	"fmt"
	"sync"
)

// Hook — the lifecycle observability surface introduced in s09. Mirrors
// upstream `nanobot/agent/hook.py::AgentHook`.
//
// Five tap points cover the full inner-loop lifecycle:
//
//   BeforeIter         — fires once per iteration, *before* the LLM call.
//                        Most useful for: telemetry, mid-flight cancel,
//                        sideband injection of extra context.
//   OnStream           — fires once per streamed delta token. Hook-supported
//                        streaming is wired in s09 via this method, but the
//                        Runner only invokes it when a hook implementation
//                        opts in (see `wantsStreaming` in upstream). Our Go
//                        version doesn't yet thread streamed deltas (that's
//                        an extension exercise), but we keep the method on
//                        the interface so future writers don't have to
//                        change `Hook` to add it.
//   BeforeExecuteTools — fires after the LLM has returned tool_calls but
//                        before we actually invoke them. Returning a
//                        non-nil error from this method short-circuits
//                        the iteration cleanly (no tools run, the run
//                        returns with the captured error).
//   AfterIter          — fires once per iteration, *after* the iteration
//                        finishes (whether it stopped, requested tools,
//                        or hit a length recovery). The HookCtx still
//                        carries the response and tool_calls from the
//                        iteration that just ended, so observability
//                        hooks can record them here.
//   FinalizeContent    — fires exactly once, after the loop ends, with the
//                        final assistant content. The hook's return value
//                        replaces the content the Runner returns to the
//                        caller. Useful for redaction, formatting, or
//                        appending sign-offs.
//
// `HookCtx` is a *mutable bag* — the Runner shares the same `*HookCtx`
// across all hook calls within a single Run. Each iteration mutates
// `Iter`, `Resp`, and `ToolCalls` before firing `BeforeIter` /
// `BeforeExecuteTools` / `AfterIter`. Hooks can stash values on it in
// addition to consuming them.
//
// Why a mutable bag instead of immutable arguments per method? It mirrors
// upstream's `AgentHookContext` dataclass and keeps the method signatures
// stable as the inner loop grows new fields (s10 adds usage drift, s11
// adds turn-state). Hooks observe what they care about and ignore the rest.

// HookCtx is the per-run mutable state shared with hook implementations.
type HookCtx struct {
	// Iter is the 0-indexed iteration counter inside Runner.Run. Reset
	// at the start of each Run; advanced by Runner before BeforeIter.
	Iter int

	// Messages is a snapshot of the messages slice as Runner sees it for
	// this iteration. The Runner refreshes this on every iteration so
	// `AfterIter` reflects the post-iteration state. SDKCaptureHook
	// snapshots from this in `AfterIter`.
	Messages []Message

	// Resp is the LLM's response for the iteration that just executed.
	// Nil during BeforeIter; populated by Runner before BeforeExecuteTools
	// and AfterIter.
	Resp *LLMResponse

	// ToolCalls is the list of tool calls the LLM requested this
	// iteration (subset of Resp.ToolCalls; the Runner may filter on the
	// way in but currently passes them through verbatim).
	ToolCalls []ToolCall

	// Usage accumulates token usage across iterations. Refreshed by the
	// Runner before AfterIter.
	Usage Usage

	// Err is non-nil when something went wrong this iteration. Reserved
	// for future use; currently nil unless a hook sets it explicitly.
	Err error
}

// Hook is the lifecycle interface. All five methods are required (no
// optional ones). Most implementations only care about a couple of
// methods; embed `NoopHook` to inherit no-op defaults for the rest.
type Hook interface {
	BeforeIter(ctx context.Context, hc *HookCtx) error
	OnStream(ctx context.Context, hc *HookCtx, delta string) error
	BeforeExecuteTools(ctx context.Context, hc *HookCtx) error
	AfterIter(ctx context.Context, hc *HookCtx) error
	FinalizeContent(ctx context.Context, hc *HookCtx, content string) string
}

// NoopHook — default Hook for Runners that don't otherwise have one set.
// Embed this in custom hooks to skip implementing methods you don't use.
type NoopHook struct{}

func (NoopHook) BeforeIter(ctx context.Context, hc *HookCtx) error         { return nil }
func (NoopHook) OnStream(ctx context.Context, hc *HookCtx, delta string) error { return nil }
func (NoopHook) BeforeExecuteTools(ctx context.Context, hc *HookCtx) error { return nil }
func (NoopHook) AfterIter(ctx context.Context, hc *HookCtx) error          { return nil }
func (NoopHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
	return content
}

// CompositeHook fans out each lifecycle method to a list of registered
// hooks. Mirrors upstream `CompositeHook`.
//
// Error isolation: BeforeIter / OnStream / BeforeExecuteTools / AfterIter
// each wrap every per-hook call in a `recover()` so a panicking hook
// does NOT abort the rest of the registered hooks. Return-value
// aggregation: bool/error-returning methods join all errors with errors.Join
// (Go 1.20+) so the caller sees every hook's verdict, not just the first.
//
// FinalizeContent is *not* error-isolated — it's a pipeline (each hook's
// output feeds the next), and a panic in a finalize hook should fail the
// run rather than silently dropping a transformation. Same trade-off
// upstream makes.
type CompositeHook struct {
	hooks []Hook

	// LogPanic is invoked when a per-hook call panics. Defaults to a
	// stderr-bound printer; tests override it to capture output.
	LogPanic func(method string, h Hook, recovered any)
}

// NewCompositeHook constructs a CompositeHook over the given hooks.
// The order of registration is preserved — hooks fire in registration
// order for every lifecycle method.
func NewCompositeHook(hs ...Hook) *CompositeHook {
	return &CompositeHook{hooks: append([]Hook(nil), hs...)}
}

// Add appends a hook to the end of the fan-out list. Safe to call at
// any time, but typically used only at construction.
func (c *CompositeHook) Add(h Hook) { c.hooks = append(c.hooks, h) }

// safeCall runs fn under a recover() so a panicking hook is logged but
// does not stop the iteration over the rest of the registered hooks.
// The error from fn (if non-nil and there's no panic) is returned.
func (c *CompositeHook) safeCall(method string, h Hook, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if c.LogPanic != nil {
				c.LogPanic(method, h, r)
			} else {
				fmt.Printf("[hook %s panic in %T]: %v\n", method, h, r)
			}
			// A panic is treated as "this hook failed" but the
			// composite continues. We do NOT propagate the panic.
			err = fmt.Errorf("hook %s panic in %T: %v", method, h, r)
		}
	}()
	return fn()
}

// joinErrs combines per-hook errors. We don't use errors.Join here so
// the s09 module stays compatible with older Go in case a learner uses
// 1.19; the implementation is explicit and easy to read.
func joinErrs(errs []error) error {
	var nonNil []error
	for _, e := range errs {
		if e != nil {
			nonNil = append(nonNil, e)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	if len(nonNil) == 1 {
		return nonNil[0]
	}
	msg := "multiple hook errors:"
	for _, e := range nonNil {
		msg += "\n  - " + e.Error()
	}
	return fmt.Errorf("%s", msg)
}

// BeforeIter fans out to every registered hook's BeforeIter. Errors are
// aggregated; panics are recovered and logged.
func (c *CompositeHook) BeforeIter(ctx context.Context, hc *HookCtx) error {
	errs := make([]error, 0, len(c.hooks))
	for _, h := range c.hooks {
		err := c.safeCall("BeforeIter", h, func() error { return h.BeforeIter(ctx, hc) })
		errs = append(errs, err)
	}
	return joinErrs(errs)
}

// OnStream fans out the streamed delta to every registered hook.
func (c *CompositeHook) OnStream(ctx context.Context, hc *HookCtx, delta string) error {
	errs := make([]error, 0, len(c.hooks))
	for _, h := range c.hooks {
		err := c.safeCall("OnStream", h, func() error { return h.OnStream(ctx, hc, delta) })
		errs = append(errs, err)
	}
	return joinErrs(errs)
}

// BeforeExecuteTools — same fan-out + error-isolation pattern. Returning
// a non-nil error here short-circuits the Runner mid-iteration.
func (c *CompositeHook) BeforeExecuteTools(ctx context.Context, hc *HookCtx) error {
	errs := make([]error, 0, len(c.hooks))
	for _, h := range c.hooks {
		err := c.safeCall("BeforeExecuteTools", h, func() error { return h.BeforeExecuteTools(ctx, hc) })
		errs = append(errs, err)
	}
	return joinErrs(errs)
}

// AfterIter — same fan-out + error-isolation pattern.
func (c *CompositeHook) AfterIter(ctx context.Context, hc *HookCtx) error {
	errs := make([]error, 0, len(c.hooks))
	for _, h := range c.hooks {
		err := c.safeCall("AfterIter", h, func() error { return h.AfterIter(ctx, hc) })
		errs = append(errs, err)
	}
	return joinErrs(errs)
}

// FinalizeContent chains the hooks: each hook's output becomes the next
// hook's input. The final return is the content the Runner hands back
// to its caller. Per upstream's contract, FinalizeContent is *not*
// error-isolated — a panic here surfaces, because finalization bugs
// should fail loudly rather than corrupt the response.
func (c *CompositeHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
	for _, h := range c.hooks {
		content = h.FinalizeContent(ctx, hc, content)
	}
	return content
}

// SDKCaptureHook is the default observability hook — accumulates the
// list of tool names called across all iterations, plus a snapshot of
// the final messages slice.
//
// Mirrors upstream `SDKCaptureHook`. SDK callers that want structured
// observability without writing their own hook simply register a
// `*SDKCaptureHook` and read its fields after `Run` returns.
//
// All field reads/writes go through the mutex because hook calls may
// fire from goroutines other than the test's main goroutine in future
// streaming designs. Today the Runner drives all hooks inline, but
// keeping the mutex makes the type race-safe by construction.
type SDKCaptureHook struct {
	// ToolsUsed is the ordered list of tool names invoked across the
	// run. Populated in AfterIter from `hc.ToolCalls`. Duplicates are
	// preserved (if the LLM calls `bash` three times, the slice has
	// three entries) — that mirrors upstream.
	ToolsUsed []string

	// FinalMessages is the messages slice as of the last AfterIter
	// firing — i.e. the end-of-turn message list the SDK caller cares
	// about. Cleared/reset on every Run if the hook is reused.
	FinalMessages []Message

	mu sync.Mutex
}

// NewSDKCaptureHook constructs an empty SDKCaptureHook ready for use.
func NewSDKCaptureHook() *SDKCaptureHook { return &SDKCaptureHook{} }

// BeforeIter is a no-op — capture hook only listens to AfterIter.
func (s *SDKCaptureHook) BeforeIter(ctx context.Context, hc *HookCtx) error { return nil }

// OnStream is a no-op — see SDKCaptureHook docstring.
func (s *SDKCaptureHook) OnStream(ctx context.Context, hc *HookCtx, delta string) error {
	return nil
}

// BeforeExecuteTools is a no-op.
func (s *SDKCaptureHook) BeforeExecuteTools(ctx context.Context, hc *HookCtx) error { return nil }

// AfterIter records the iteration's tool calls and refreshes the
// FinalMessages snapshot.
func (s *SDKCaptureHook) AfterIter(ctx context.Context, hc *HookCtx) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range hc.ToolCalls {
		s.ToolsUsed = append(s.ToolsUsed, c.Name)
	}
	if hc.Messages != nil {
		s.FinalMessages = append([]Message(nil), hc.Messages...)
	}
	return nil
}

// FinalizeContent passes the content through unchanged.
func (s *SDKCaptureHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
	return content
}

// ToolsUsedSnapshot returns a defensive copy of ToolsUsed for callers
// that may modify it. Tests use this to assert ordering without races.
func (s *SDKCaptureHook) ToolsUsedSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ToolsUsed...)
}

// FinalMessagesSnapshot returns a defensive copy of FinalMessages.
func (s *SDKCaptureHook) FinalMessagesSnapshot() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.FinalMessages...)
}
