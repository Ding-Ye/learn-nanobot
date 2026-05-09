# Upstream reading: nanobot/agent/hook.py (annotated for s09 — Hook system)
#
# Source: https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/hook.py
# Why this file: it defines the entire lifecycle observability surface
# (AgentHook + CompositeHook + SDKCaptureHook). s09 ports this verbatim
# in shape; the only Go-flavored differences are noted inline.
#
# Reading guide:
#   1. AgentHookContext  — the mutable bag shared with hooks (~10 fields).
#   2. AgentHook         — the 5-method base interface. Most are no-ops;
#                          subclasses override what they care about.
#   3. CompositeHook     — the fan-out + error-isolation primitive. Read
#                          `_for_each_hook_safe` carefully — that's the
#                          policy that lets one bad hook not kill others.
#   4. SDKCaptureHook    — the default observability hook the SDK exports.
#                          Only `after_iteration` is overridden.
#
# Annotated excerpt (lines ~1-123 of nanobot/agent/hook.py):

"""Shared lifecycle hook primitives for agent runs."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from loguru import logger  # learn-Go: replaced by fmt.Printf or LogPanic.

from nanobot.providers.base import LLMResponse, ToolCallRequest


@dataclass(slots=True)
class AgentHookContext:
    """Mutable per-iteration state exposed to runner hooks."""
    # learn-Go: HookCtx in hook.go. We keep iteration/messages/response/usage
    # /tool_calls/error and skip the streaming-specific fields (tool_results,
    # tool_events, streamed_content, stop_reason, final_content). The Go
    # Runner doesn't yet drive streaming, so those would be unused fields.

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


class AgentHook:
    """Minimal lifecycle surface for shared runner customization."""
    # learn-Go: Hook interface in hook.go. Method signatures become Go
    # idiomatic (ctx context.Context, hc *HookCtx, ... -> error). The
    # async-vs-sync split disappears: Go hooks are sync; if a hook needs
    # I/O it spawns a goroutine.

    def __init__(self, reraise: bool = False) -> None:
        # learn-Go: dropped. Tests that want re-raise semantics register
        # the hook directly without going through CompositeHook.
        self._reraise = reraise

    def wants_streaming(self) -> bool:
        # learn-Go: dropped. Streaming wiring is an extension exercise.
        # Future Go streaming can use a nil check on the OnStream method
        # the same way Python uses this opt-in flag.
        return False

    async def before_iteration(self, context: AgentHookContext) -> None:
        # learn-Go: BeforeIter(ctx, hc) error. Default is no-op.
        pass

    async def on_stream(self, context: AgentHookContext, delta: str) -> None:
        # learn-Go: OnStream(ctx, hc, delta) error. The Go Runner doesn't
        # invoke this yet (streaming isn't wired); we keep it on the
        # interface so future writers can add streaming without breaking
        # every existing Hook implementation.
        pass

    async def on_stream_end(self, context: AgentHookContext, *, resuming: bool) -> None:
        # learn-Go: collapsed into OnStream. We don't need the
        # "iteration-end while streaming" signal until streaming itself
        # is wired.
        pass

    async def before_execute_tools(self, context: AgentHookContext) -> None:
        # learn-Go: BeforeExecuteTools(ctx, hc) error. Returning a
        # non-nil error short-circuits the iteration cleanly (the Runner
        # bails out without calling tools; AfterIter still fires for
        # observability hooks; the run returns the wrapped error).
        pass

    async def after_iteration(self, context: AgentHookContext) -> None:
        # learn-Go: AfterIter(ctx, hc) error. Always fires per iteration,
        # regardless of how the iteration ended. SDKCaptureHook records
        # tool_calls + messages here.
        pass

    def finalize_content(self, context: AgentHookContext, content: str | None) -> str | None:
        # learn-Go: FinalizeContent(ctx, hc, content) string. Sync (not
        # async/error-returning) because finalization happens after I/O
        # is done, with a final string in hand. The hook's return value
        # replaces what the Runner hands the caller.
        return content


class CompositeHook(AgentHook):
    """Fan-out hook that delegates to an ordered list of hooks.

    Error isolation: async methods catch and log per-hook exceptions
    so a faulty custom hook cannot crash the agent loop.
    ``finalize_content`` is a pipeline (no isolation — bugs should surface).
    """
    # learn-Go: CompositeHook in hook.go. The Go version uses recover()
    # per-hook-per-method to translate Python's try/except. Same policy:
    # observer methods are isolated; FinalizeContent is a pipeline that
    # bubbles panics up.

    __slots__ = ("_hooks",)

    def __init__(self, hooks: list[AgentHook]) -> None:
        super().__init__()
        self._hooks = list(hooks)

    def wants_streaming(self) -> bool:
        return any(h.wants_streaming() for h in self._hooks)

    async def _for_each_hook_safe(self, method_name: str, *args: Any, **kwargs: Any) -> None:
        # learn-Go: see CompositeHook.safeCall in hook.go. Python's
        # try/except becomes Go's defer + recover; logger.exception
        # becomes c.LogPanic (a callable so tests can capture output).
        for h in self._hooks:
            if getattr(h, "_reraise", False):
                # learn-Go: not implemented. Tests that want re-raise
                # register the hook directly without the composite.
                await getattr(h, method_name)(*args, **kwargs)
                continue

            try:
                await getattr(h, method_name)(*args, **kwargs)
            except Exception:
                # The composite logs and CONTINUES. Other hooks for the
                # same method still run. This is the central isolation
                # invariant.
                logger.exception("AgentHook.{} error in {}", method_name, type(h).__name__)

    async def before_iteration(self, context: AgentHookContext) -> None:
        await self._for_each_hook_safe("before_iteration", context)

    async def on_stream(self, context: AgentHookContext, delta: str) -> None:
        await self._for_each_hook_safe("on_stream", context, delta)

    async def on_stream_end(self, context: AgentHookContext, *, resuming: bool) -> None:
        await self._for_each_hook_safe("on_stream_end", context, resuming=resuming)

    async def before_execute_tools(self, context: AgentHookContext) -> None:
        await self._for_each_hook_safe("before_execute_tools", context)

    async def after_iteration(self, context: AgentHookContext) -> None:
        await self._for_each_hook_safe("after_iteration", context)

    def finalize_content(self, context: AgentHookContext, content: str | None) -> str | None:
        # learn-Go: pipeline (no recover). Each hook's output becomes the
        # next hook's input. A panic here surfaces — finalize bugs should
        # fail loudly rather than corrupt the response.
        for h in self._hooks:
            content = h.finalize_content(context, content)
        return content


class SDKCaptureHook(AgentHook):
    """Record tool names and the final message list for ``RunResult``.

    The runner mutates ``context.messages`` in place across iterations, so the
    snapshot is refreshed on every ``after_iteration`` call; the last call
    reflects the end-of-turn state the SDK caller cares about.
    """
    # learn-Go: SDKCaptureHook in hook.go. Goes through every Hook method
    # explicitly (no-op for the four it doesn't care about; AfterIter for
    # the one it does). Tests use ToolsUsedSnapshot() and
    # FinalMessagesSnapshot() to read with mutex protection.

    def __init__(self) -> None:
        super().__init__()
        self.tools_used: list[str] = []
        self.messages: list[dict[str, Any]] = []

    async def after_iteration(self, context: AgentHookContext) -> None:
        # learn-Go: ToolsUsed = append(ToolsUsed, c.Name) for each
        # hc.ToolCalls; FinalMessages snapshotted via append([]Message(nil),
        # hc.Messages...). Both protected by sync.Mutex for race safety.
        for call in context.tool_calls:
            self.tools_used.append(call.name)
        # Re-snapshot every iteration; the last AfterIter wins.
        self.messages = list(context.messages)


# ─── Things to notice on a careful read ─────────────────────────────────────
#
# 1. The hook context is *mutable*. The runner shares one AgentHookContext
#    with every hook call within an iteration. That keeps the method
#    signatures stable as fields grow (s10/s11 will add more), but means
#    hooks must be careful: snapshotting a list field requires a copy
#    (Python: list(...); Go: append([]X(nil), src...)).
#
# 2. CompositeHook has a `_reraise` opt-out per hook (we drop in learn-Go).
#    In practice it's used by tests that need to assert errors. The Go
#    test suite gets the same behavior by registering a hook directly on
#    the runner (without going through the composite).
#
# 3. SDKCaptureHook duplicates entries in tools_used. If the LLM calls
#    `bash` three times, the slice has three entries. This mirrors what a
#    "honest tool use audit log" should look like; the Runner separately
#    deduplicates for AgentRunResult.ToolsUsed (set-like semantics for
#    "what tools did this turn touch?").
#
# 4. wants_streaming() is the Python ergonomic for opting into OnStream
#    callbacks. The Runner only invokes OnStream if the hook (or any
#    hook in a composite) returns True from wants_streaming. The Go
#    version doesn't yet drive streaming, so we don't ship the opt-in.
#    Future streaming wiring can either (a) recur on every Hook impl
#    being non-NoopHook (cheap), or (b) add a `WantsStreaming() bool`
#    method (faithful).
#
# 5. The one method that is sync rather than async is finalize_content.
#    Why? Because it runs after all the I/O is done. The Go version
#    inherits the same synchronous-by-default contract for free.
#
# ─── Cross-references to learn-go ───────────────────────────────────────────
#
# - AgentHookContext  → HookCtx (hook.go:32)
# - AgentHook         → Hook interface (hook.go:55)
# - CompositeHook     → CompositeHook struct (hook.go:88)
# - SDKCaptureHook    → SDKCaptureHook struct (hook.go:189)
# - _for_each_hook_safe → CompositeHook.safeCall (hook.go:104)
# - finalize_content  → FinalizeContent on Hook (hook.go:179, hook.go:53)
#
# ─── Reading order suggestion ───────────────────────────────────────────────
#
# 1. Read AgentHook (lines 30-55) to see the surface.
# 2. Read CompositeHook._for_each_hook_safe (75-85) to see the policy.
# 3. Read SDKCaptureHook.after_iteration (120-123) to see what a real
#    custom hook looks like.
# 4. Skim runner.py:_run_iteration in nanobot/agent/runner.py to see
#    where each hook fires.
