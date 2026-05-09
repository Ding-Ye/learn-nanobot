---
title: "s09 · Hook system"
chapter: 9
slug: s09-hooks
est_read_min: 22
---

# s09 · Hook system

> What this teaches: real agents need lifecycle observability. The Runner has been a "black box" through s08 — call it, get a result, hope nothing broke. s09 cracks it open with five tap points: `BeforeIter`, `OnStream`, `BeforeExecuteTools`, `AfterIter`, `FinalizeContent`. `CompositeHook` fans out to many hooks with **error isolation** (one panicking hook can't break the others). `SDKCaptureHook` is the "default observability hook" upstream provides so SDK callers can read `tools_used` + final messages without writing their own. Hooks are the seam where streaming, telemetry, mid-flight cancel, and sideband injection plug in (all extension exercises).

---

## Problem

Through s08 the Runner is a closed loop. You hand it `RunSpec.InitialMessages + RunSpec.System + RunSpec.Tools`, you get back `AgentRunResult.FinalContent + Messages + ToolsUsed + Usage`. If you want to:

- log every iteration's token usage to a metrics endpoint;
- stream partial assistant tokens to a websocket so the UI shows progress;
- cancel a turn mid-flight when the user clicks "stop";
- inject a sideband instruction ("the user just sent a follow-up while you were thinking — please incorporate") between iterations;
- redact PII from the final response before it goes back to the user;

— you can't, today. There's no hook into the loop. The Runner doesn't fire any callbacks. The only signal is the return value.

Upstream's answer is `nanobot/agent/hook.py`. It defines `AgentHook`, an interface with five lifecycle methods (plus an `on_stream_end` we collapse into `OnStream` for s09's MVP), and an `AgentHookContext` mutable bag the Runner shares with hook implementations. `CompositeHook` lets you stack many hooks on one Runner. `SDKCaptureHook` is the default observability hook the SDK ships so library callers don't have to write one.

The interesting part isn't the interface — it's the **error isolation** policy. If one custom hook panics in `before_iteration`, the run shouldn't die. Other registered hooks should still see their callbacks. The composite catches exceptions per-hook-per-method and logs them. There's one exception to that rule: `finalize_content` is a *pipeline* (each hook's output is the next hook's input), and a panic there should fail loudly because finalization bugs corrupt the response in ways that matter.

s09 ports that policy verbatim, with one Go-flavored detail: panics replace `try/except`. The composite uses `recover()` per-hook-per-method, logs the recovered value, and surfaces an aggregated error to the Runner so the caller still hears about it.

The tricky test design choice: the recording hook needs to see exactly the right call sequence across iterations. Mismatched ordering would silently break observability hooks in production. So the first test pins `[BeforeIter(0), BeforeExecuteTools(0), AfterIter(0), BeforeIter(1), AfterIter(1), FinalizeContent]` for a 2-turn loop. If you later refactor the Runner and accidentally fire `AfterIter` before tools execute, this test catches you.

## Solution

Three new types in `hook.go`:

```go
type Hook interface {
    BeforeIter(ctx context.Context, hc *HookCtx) error
    OnStream(ctx context.Context, hc *HookCtx, delta string) error
    BeforeExecuteTools(ctx context.Context, hc *HookCtx) error
    AfterIter(ctx context.Context, hc *HookCtx) error
    FinalizeContent(ctx context.Context, hc *HookCtx, content string) string
}

type HookCtx struct {
    Iter      int
    Messages  []Message
    Resp      *LLMResponse
    ToolCalls []ToolCall
    Usage     Usage
    Err       error
}

type CompositeHook struct {
    hooks    []Hook
    LogPanic func(method string, h Hook, recovered any)
}
```

Plus three small helpers:

```go
type NoopHook struct{}            // default for Runners without a hook
type SDKCaptureHook struct {      // upstream's default observability hook
    ToolsUsed     []string
    FinalMessages []Message
}
func NewCompositeHook(hs ...Hook) *CompositeHook
func (c *CompositeHook) Add(h Hook)
```

`Runner` gains a `Hook` field. `Runner.Run` calls hooks at five well-defined moments per iteration:

1. `BeforeIter` — top of every iteration, before the LLM call. Returning a non-nil error short-circuits the run.
2. (LLM call)
3. `BeforeExecuteTools` — only fires when the LLM asked for tools. Returning a non-nil error short-circuits the iteration cleanly (no tools run).
4. (Tools execute)
5. `AfterIter` — bottom of every iteration, regardless of how it ended (stop / tool_calls / length / error).

After the loop ends, once:

6. `FinalizeContent` — the hook's return value replaces what the Runner hands the caller.

`OnStream` is on the interface but the Runner doesn't yet invoke it — streaming is an extension exercise. The method exists so future writers don't have to widen the interface to add it.

The three test fixtures cover the full surface:

- `recordingHook` records every method as `Method(iter)` for ordering assertions.
- `panicHook` panics in `BeforeIter` to test error isolation.
- `rewriteHook` (embeds `NoopHook`) prepends "rewritten:" in `FinalizeContent`.
- `shortcircuitHook` (embeds `NoopHook`) returns a sentinel from `BeforeExecuteTools`.

## How It Works

```ascii-anim frames=4
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  Runner.Run                                                      │
│    hook := r.Hook                                                │
│    if hook == nil { hook = NoopHook{} }                          │
│    hc := &HookCtx{}                                              │
│                                                                  │
│    for iter := 0; iter < MaxIterations; iter++ {                 │
│        hc.Iter = iter                                            │
│        hc.Messages = messages                                    │
│        hc.Resp = nil; hc.ToolCalls = nil                         │
│                                                                  │
│        ╔══ BeforeIter ══╗  ← tap point #1                        │
│        if err := hook.BeforeIter(ctx, hc); err != nil { return } │
│                                                                  │
│        resp, _ := provider.Chat(ctx, ...)                        │
│        hc.Resp = &resp; hc.ToolCalls = resp.ToolCalls            │
│        hc.Messages = messages // refreshed                       │
│                                                                  │
│        switch resp.FinishReason {                                │
│        case "stop":                                              │
│            ╔══ AfterIter ══╗  ← tap point #5                     │
│            hook.AfterIter(ctx, hc)                               │
│            ╔══ FinalizeContent ══╗  ← tap point #6               │
│            return hook.FinalizeContent(ctx, hc, resp.Content)    │
│                                                                  │
│        case "tool_calls":                                        │
│            ╔══ BeforeExecuteTools ══╗  ← tap point #3            │
│            if err := hook.BeforeExecuteTools(...); err != nil {  │
│                hook.AfterIter(...)  // still fire for telemetry  │
│                return                                            │
│            }                                                     │
│            // run tools, append results to messages              │
│        }                                                         │
│        ╔══ AfterIter ══╗  ← tap point #5 (every iter)            │
│        hook.AfterIter(ctx, hc)                                   │
│    }                                                             │
│                                                                  │
│  CompositeHook fan-out (per non-finalize method):                │
│    for h := range c.hooks {                                      │
│        defer func() {                                            │
│            if r := recover(); r != nil {                         │
│                c.LogPanic(method, h, r)                          │
│                err = fmt.Errorf("hook %s panic in %T: %v", ...)  │
│            }                                                     │
│        }()                                                       │
│        err := h.method(ctx, hc, ...)                             │
│        errs = append(errs, err)                                  │
│    }                                                             │
│    return joinErrs(errs)                                         │
│                                                                  │
│  CompositeHook.FinalizeContent (pipeline, no recover):           │
│    for h := range c.hooks {                                      │
│        content = h.FinalizeContent(ctx, hc, content)             │
│    }                                                             │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

Core ~40 lines (excerpted from [`agents/s09-hooks/hook.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s09-hooks/hook.go)):

```go
func (c *CompositeHook) safeCall(method string, h Hook, fn func() error) (err error) {
    defer func() {
        if r := recover(); r != nil {
            if c.LogPanic != nil {
                c.LogPanic(method, h, r)
            } else {
                fmt.Printf("[hook %s panic in %T]: %v\n", method, h, r)
            }
            err = fmt.Errorf("hook %s panic in %T: %v", method, h, r)
        }
    }()
    return fn()
}

func (c *CompositeHook) BeforeIter(ctx context.Context, hc *HookCtx) error {
    errs := make([]error, 0, len(c.hooks))
    for _, h := range c.hooks {
        err := c.safeCall("BeforeIter", h, func() error { return h.BeforeIter(ctx, hc) })
        errs = append(errs, err)
    }
    return joinErrs(errs)
}

func (c *CompositeHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
    for _, h := range c.hooks {
        content = h.FinalizeContent(ctx, hc, content)
    }
    return content
}
```

**5 non-obvious points**:

1. **`HookCtx` is a mutable bag, not immutable arguments.** Mirrors upstream's `AgentHookContext` dataclass. The Runner mutates the same `*HookCtx` across all hook calls within one Run. This keeps the method signatures stable as fields are added (s10 would extend with usage drift; s11 with turn state). Hooks observe what they care about and ignore the rest.

2. **Error isolation is per-method, not blanket.** Fan-out methods recover panics; `FinalizeContent` does not. Why the asymmetry? Because finalization is a pipeline — each hook's output feeds the next. If a hook in the middle panics, dropping it silently means downstream hooks see corrupted input. Finalization bugs should fail loudly. Non-finalize methods are observers, so isolating them is safe and desirable.

3. **`AfterIter` always fires, even on errors.** A naive implementation would skip `AfterIter` when `BeforeExecuteTools` returns an error. We deliberately fire it anyway, so observability hooks see the failed iteration. The `_` in `_ = hook.AfterIter(...)` says "we don't care about its error return; the iteration is already failing for another reason".

4. **`OnStream` is on the interface but unused today.** Keeping it on the interface means the Runner can grow streaming support in a future session without breaking every existing hook implementation. If we left it off and added it later, every `Hook` implementation would have to be updated. The cost of an unused method is one no-op line per hook; the cost of an interface change is broken downstream code.

5. **The Runner uses a single `Hook`, not a `[]Hook`.** Composition is via `CompositeHook`, not via the Runner. This keeps the Runner simple (one tap call per moment, not a loop) and makes the composition explicit at the call site. Want telemetry + capture? `runner.SetHook(NewCompositeHook(telemetry, capture))`. Want capture only? `runner.SetHook(capture)`. The composite is the canonical "many hooks at once" pattern.

## What Changed

```diff
  // s08/runner.go: Runner has just Provider; Run does not call any hooks.
- type Runner struct { Provider Provider }
- func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
-     for iter := 0; iter < spec.MaxIterations; iter++ {
-         resp, err := r.Provider.Chat(ctx, ChatReq{...})
-         // run tools, etc.
-     }
- }

  // s09/runner.go: Runner gains a Hook; Run calls hooks at five points.
+ type Runner struct {
+     Provider Provider
+     Hook     Hook
+ }
+ func (r *Runner) SetHook(h Hook) { r.Hook = h }
+ func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
+     hook := r.Hook
+     if hook == nil { hook = NoopHook{} }
+     hc := &HookCtx{}
+     for iter := 0; iter < spec.MaxIterations; iter++ {
+         hc.Iter = iter
+         if err := hook.BeforeIter(ctx, hc); err != nil { ... }
+         resp, _ := r.Provider.Chat(...)
+         hc.Resp = &resp; hc.ToolCalls = resp.ToolCalls
+         if resp.FinishReason == "tool_calls" {
+             if err := hook.BeforeExecuteTools(ctx, hc); err != nil { ... }
+             // run tools
+         }
+         hook.AfterIter(ctx, hc)
+     }
+     finalContent = hook.FinalizeContent(ctx, hc, finalContent)
+ }
```

New types (the heart of s09):

```go
+ type Hook interface { ... 5 methods ... }
+ type HookCtx struct { Iter int; Messages []Message; Resp *LLMResponse; ... }
+ type NoopHook struct{}
+ type CompositeHook struct { hooks []Hook; LogPanic func(...) }
+ type SDKCaptureHook struct { ToolsUsed []string; FinalMessages []Message; mu sync.Mutex }
```

`Provider` / `Tool` / `Registry` / `Bus` / `SessionManager` / `MemoryStore` / `SkillLoader` / `ContextBuilder` are all unchanged. s09's only wiring change is `Runner.Hook` and the five tap-point insertions.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s09-hooks

# Run with verbose logging — watch the new [s09] hook recorded line.
go run . -workspace /tmp/agent-demo -skills weather -v "Tokyo weather"
# [s09] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s09] loaded 2 skill(s) ...
# [s09] system prompt: 612 chars; messages: 1
# [iter 0] -> bash {command:curl -s wttr.in/Tokyo?format=3}
# [iter 0] <- Tokyo: clear, 18°C
# [iter 0] assistant: ...
# [s09] hook recorded 1 tool invocation(s): [bash]
# Tokyo: clear, 18°C.
```

The `[s09] hook recorded ...` line is the externally-visible signal that SDKCaptureHook ran. Internally, the same hook also captured `FinalMessages` for the SDK caller's convenience.

Run the tests (race detector mandatory):

```bash
go test -count=1 -race -v ./...
```

6 tests should pass:

- `TestHook_BeforeIter_AfterIter_CalledInOrder` — pins the call sequence;
- `TestCompositeHook_OneFails_OthersStillRun` — verifies error isolation;
- `TestSDKCaptureHook_AccumulatesToolsUsed` — verifies the default observability hook;
- `TestHook_FinalizeContent_CanRewrite` — verifies FinalizeContent can rewrite;
- `TestHook_BeforeExecuteTools_CanShortcircuit` — verifies short-circuit semantics;
- `TestRunner_NoHook_DefaultsToNoop` — sanity check that the Runner works without a hook.

## Upstream Source Reading

s09 corresponds to `nanobot/agent/hook.py` — the whole file is ~123 lines. We translate `AgentHookContext`, `AgentHook`, `CompositeHook`, and `SDKCaptureHook` to Go. We collapse `on_stream_end` into `OnStream` (the runner doesn't yet drive streaming), and we drop `wants_streaming()` (it's a Python ergonomic — a hook says "fire OnStream for me"; Go's nil-check on the future streaming wiring will do the same job).

Annotated version in [`upstream-readings/s09-hooks.py`](../../upstream-readings/s09-hooks.py).

```upstream:nanobot/agent/hook.py#L13-L27
@dataclass(slots=True)
class AgentHookContext:
    """Mutable per-iteration state exposed to runner hooks."""

    iteration: int
    messages: list[dict[str, Any]]
    response: LLMResponse | None = None
    usage: dict[str, int] = field(default_factory=dict)
    tool_calls: list[ToolCallRequest] = field(default_factory=list)
    tool_results: list[Any] = field(default_factory=list)
    tool_events: list[dict[str, str]] = field(default_factory=list)
    streamed_content: bool = False
    final_content: str | None = None
    stop_reason: str | None = None
    error: str | None = None
```

```upstream:nanobot/agent/hook.py#L75-L85
async def _for_each_hook_safe(self, method_name: str, *args: Any, **kwargs: Any) -> None:
    for h in self._hooks:
        if getattr(h, "_reraise", False):
            await getattr(h, method_name)(*args, **kwargs)
            continue

        try:
            await getattr(h, method_name)(*args, **kwargs)
        except Exception:
            logger.exception("AgentHook.{} error in {}", method_name, type(h).__name__)
```

**5 cross-reading takeaways**:

- **Upstream's `AgentHookContext` has more fields than learn's `HookCtx`** — `tool_results`, `tool_events`, `streamed_content`, `stop_reason`, `final_content`. We picked the subset our Runner actually populates and used. `tool_results` is implicit in `Messages` (the runner appends them as user-role tool_result blocks); `tool_events` is for streaming MCP tools we haven't built; `streamed_content` is a streaming flag we don't need yet. The interface is one method-add away from full parity if a future session needs it.

- **Upstream's hooks are `async`**; Go's are sync. Go's calling convention is "hooks block the Runner until they return". If a hook needs to do I/O, it spawns a goroutine. Mirrors how Go usually translates Python's `async def` — the synchronous-by-default contract is simpler to teach.

- **Upstream supports a `_reraise` flag per hook**; learn doesn't. Setting `_reraise=True` on a custom hook tells `CompositeHook` to bypass the try/except and let the exception propagate. Useful for tests that *want* the error to fail the run. We don't ship the flag because tests can register the hook directly (without the composite) when they want re-raise semantics.

- **Upstream's `SDKCaptureHook` only implements `after_iteration`**, not the full interface. Other methods inherit no-op defaults from `AgentHook.__init__`. Go has no inheritance for interfaces, so we either explicitly implement every method on `SDKCaptureHook` (what s09 does) or embed a `NoopHook` for free defaults. Both are valid; we went explicit because it's more searchable in code review (you can grep for "SDKCaptureHook" and see every method it implements).

- **Upstream's `finalize_content` is sync, not async** — the only sync method on `AgentHook`. The reason is that finalization happens after all I/O is done, with a final string in hand. There's nothing to await. Our Go version inherits the same synchronous-by-default contract for free.

**For deeper reading**:

1. `nanobot/agent/runner.py:_run_iteration` — the upstream Runner's hook integration. Same five tap points, but the surrounding control flow is also responsible for streaming (`_run_streamed_iteration` vs `_run_blocking_iteration`). Reading this tells you why `wants_streaming()` exists in the upstream interface.

2. `nanobot/agent/runner.py:_handle_streamed_iteration` — the streaming path. Hook's `on_stream(delta)` is called once per delta token; `on_stream_end(resuming=True)` fires when a streaming iteration ends but the loop will continue (because the LLM wants tools).

3. `nanobot/utils/audit.py` — production audit hook. A real-world hook implementation that logs every iteration's prompt + response to disk. Reading this gives you a feel for what custom hooks look like in production: they hold open file handles, batch writes, and fail-safe on disk full.

---

**Next**: s10 — Consolidator + AutoCompact for memory compression.

Next: s10 — Consolidator + AutoCompact for memory compression.
