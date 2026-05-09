# Source: HKUDS/nanobot · nanobot/agent/runner.py
# License: MIT
# Fetched: 2026-05-09 from main branch
#
# Teaching excerpt for learn-nanobot/s04-agent-runner.
# The full upstream file is ~250 LOC and is the densest part of the agent
# core. This excerpt keeps the dataclasses + the iteration loop body —
# enough to map onto our Go Runner. Skipped: injection sideband, streaming
# callbacks, micro-compaction, checkpointing, retry policy. Each of those
# is a later chapter.

# ────────────────────────────────────────────────────────────────────
# Part 1. Constants & module-level config
# ────────────────────────────────────────────────────────────────────

_DEFAULT_ERROR_MESSAGE = "Sorry, I encountered an error calling the AI model."
_MAX_EMPTY_RETRIES = 2
_MAX_LENGTH_RECOVERIES = 3   # ← learn-nanobot/s04 ships 1 of these
_MAX_INJECTIONS_PER_TURN = 3
_MAX_INJECTION_CYCLES = 5


# ────────────────────────────────────────────────────────────────────
# Part 2. AgentRunSpec — what a single agent execution needs
# ────────────────────────────────────────────────────────────────────

@dataclass(slots=True)
class AgentRunSpec:
    """Configuration for a single agent execution."""

    # Core 5 — these are what learn-nanobot/s04's AgentRunSpec also has.
    initial_messages: list[dict[str, Any]]   # caller-built (NOT runner)
    tools: ToolRegistry
    model: str
    max_iterations: int
    max_tool_result_chars: int               # we drop this; s10 will add
                                             # tool-result truncation

    # Provider knobs — Phase G can route these per-provider.
    temperature: float | None = None
    max_tokens: int | None = None
    reasoning_effort: str | None = None

    # Hook (s09) — fires at before_iteration / on_stream /
    # before_execute_tools / after_iteration / finalize_content.
    hook: AgentHook | None = None

    # Error UX — what to say to the user when we bail.
    error_message: str | None = _DEFAULT_ERROR_MESSAGE
    max_iterations_message: str | None = None

    # Tool execution mode.
    concurrent_tools: bool = False    # we run sequential; gather() upstream
    fail_on_tool_error: bool = False  # we always continue with "tool error"

    # Workspace / session — used by sandboxed exec, by tools that need
    # session context (s05+).
    workspace: Path | None = None
    session_key: str | None = None

    # Context budgeting — used by a "should we compact?" check (s10).
    context_window_tokens: int | None = None
    context_block_limit: int | None = None

    # Provider retry policy. Nanobot supports "standard" (centralized)
    # vs "passthrough" (Provider does its own retries).
    provider_retry_mode: str = "standard"

    # Sideband — UI sends an extra user message mid-turn, or asks Runner
    # to wait/checkpoint. We don't expose these in s04.
    progress_callback: Any | None = None
    stream_progress_deltas: bool = True
    retry_wait_callback: Any | None = None
    checkpoint_callback: Any | None = None
    injection_callback: Any | None = None
    llm_timeout_s: float | None = None


# ────────────────────────────────────────────────────────────────────
# Part 3. AgentRunResult — what the run gives back
# ────────────────────────────────────────────────────────────────────

@dataclass(slots=True)
class AgentRunResult:
    """Outcome of a shared agent execution."""

    final_content: str | None                # ↔ Go: FinalContent
    messages: list[dict[str, Any]]           # ↔ Go: Messages (full log)
    tools_used: list[str] = field(default_factory=list)        # ↔ ToolsUsed
    usage: dict[str, int] = field(default_factory=dict)        # ↔ Usage
    stop_reason: str = "completed"           # ↔ FinishReason ("stop" in Go)
    error: str | None = None                  # learn-nanobot uses Go error
    tool_events: list[dict[str, str]] = field(default_factory=list)
    had_injections: bool = False             # only true when sideband fires


# ────────────────────────────────────────────────────────────────────
# Part 4. AgentRunner.run — the core loop (simplified body)
# ────────────────────────────────────────────────────────────────────

class AgentRunner:
    """Run a tool-capable LLM loop without product-layer concerns."""

    def __init__(self, provider: LLMProvider):
        self.provider = provider

    async def run(self, spec: AgentRunSpec) -> AgentRunResult:
        # State for one whole run. Most of these are tracked across
        # iterations; only `iteration` resets per-loop.
        hook = spec.hook or AgentHook()
        messages = list(spec.initial_messages)
        final_content: str | None = None
        tools_used: list[str] = []
        usage: dict[str, int] = {"prompt_tokens": 0, "completion_tokens": 0}
        error: str | None = None
        stop_reason = "completed"
        tool_events: list[dict[str, str]] = []
        empty_content_retries = 0
        length_recovery_count = 0     # ← _MAX_LENGTH_RECOVERIES gate
        had_injections = False
        injection_cycles = 0

        for iteration in range(spec.max_iterations):
            # ── Hook: before_iteration ─────────────────────────────────
            # learn-nanobot/s09 will add this back; s04 has no hook.
            await hook.before_iteration(
                AgentHookContext(iteration=iteration, messages=messages, ...)
            )

            # ── Provider call (with built-in transient retry) ─────────
            response: LLMResponse = await self.provider.chat(
                messages=messages,
                tools=spec.tools.get_definitions(),
                model=spec.model,
                max_tokens=spec.max_tokens,
                temperature=spec.temperature,
                ...
            )

            # ── Bookkeeping: tally usage ─────────────────────────────
            usage["prompt_tokens"] += response.usage.get("input_tokens", 0)
            usage["completion_tokens"] += response.usage.get("output_tokens", 0)

            # ── Append assistant message to history (always) ─────────
            assistant_message = build_assistant_message(response)
            # NOTE: we DON'T append yet if injections might intercept.
            # learn-nanobot/s04 always appends; that's a simplification.

            # ── Branch on finish_reason ──────────────────────────────
            if response.finish_reason == "stop" and not response.has_tool_calls:
                # Empty content guard: if the model returned blank text,
                # retry up to _MAX_EMPTY_RETRIES with a "please respond"
                # hint. learn-nanobot doesn't do this yet.
                if is_blank_text(response.content):
                    empty_content_retries += 1
                    if empty_content_retries < _MAX_EMPTY_RETRIES:
                        messages.append(build_finalization_retry_message())
                        continue
                # Done. Persist and return.
                final_content = response.content
                messages.append(assistant_message)
                stop_reason = "completed"
                break

            elif response.has_tool_calls:
                # ── Tool execution ───────────────────────────────────
                messages.append(assistant_message)
                if spec.concurrent_tools:
                    results = await asyncio.gather(*[
                        spec.tools.execute(c.name, c.arguments)
                        for c in response.tool_calls
                    ])
                else:
                    results = []
                    for c in response.tool_calls:
                        try:
                            r = await spec.tools.execute(c.name, c.arguments)
                        except Exception as e:
                            if spec.fail_on_tool_error:
                                raise
                            r = f"tool error: {e}"   # ← same as our Registry.Execute
                        results.append(r)
                # Append tool_result content blocks (one per call).
                tool_results_msg = {
                    "role": "user",
                    "content": [
                        {"type": "tool_result",
                         "tool_use_id": c.id,
                         "content": maybe_persist_tool_result(r, spec)}
                        for c, r in zip(response.tool_calls, results)
                    ],
                }
                messages.append(tool_results_msg)
                # Track tools_used (de-duplicated, ordered).
                for c in response.tool_calls:
                    if c.name not in tools_used:
                        tools_used.append(c.name)
                continue

            elif response.finish_reason == "length":
                # ── Length recovery (we ship 1 attempt; upstream 3) ──
                if length_recovery_count >= _MAX_LENGTH_RECOVERIES:
                    final_content = response.content    # partial
                    stop_reason = "length"
                    error = "Response exceeded length limit."
                    break
                length_recovery_count += 1
                messages.append(assistant_message)
                # build_length_recovery_message includes the truncated
                # reply re-quoted, so the model has its own context.
                messages.append(build_length_recovery_message(response.content))
                continue

            elif response.finish_reason == "error":
                # Provider returned an error response (rate limit, etc).
                # Nanobot's "standard" retry mode handled transient ones
                # in provider.chat() already; if we got here it's
                # permanent. Bail.
                error = response.error_message or _DEFAULT_ERROR_MESSAGE
                stop_reason = "error"
                break

        # ── Hook: finalize_content (rewrite the final string) ────────
        # s09 lets a hook munge the final reply; s04 has no hook.
        if final_content is not None:
            final_content = await hook.finalize_content(
                AgentHookContext(messages=messages, ...),
                final_content,
            )

        return AgentRunResult(
            final_content=final_content,
            messages=messages,
            tools_used=tools_used,
            usage=usage,
            stop_reason=stop_reason,
            error=error,
            tool_events=tool_events,
            had_injections=had_injections,
        )


# ────────────────────────────────────────────────────────────────────
# What this excerpt teaches (vs. learn-nanobot/s04)
# ────────────────────────────────────────────────────────────────────
#
# 1. Same skeleton: a `for iteration in range(max_iterations)` loop with
#    a 3-way switch on response.finish_reason → stop / tool_calls /
#    length. learn-nanobot/s04/runner.go matches this exactly.
#
# 2. Same dataclasses: AgentRunSpec.initial_messages (caller pre-builds!)
#    and AgentRunResult.{final_content, messages, tools_used, usage}.
#    The Go versions are slimmer but the field names line up.
#
# 3. Length recovery: we ship 1 attempt; upstream allows 3 and uses a
#    template hint that re-quotes the truncated reply. Production can
#    raise `lengthRecoveriesLeft` to 3 + use a real template.
#
# 4. Tool errors: Registry.Execute wraps Go errors as "tool error: <msg>"
#    so the Runner never aborts on a tool failure — same idea as
#    upstream's `f"tool error: {e}"` (with `fail_on_tool_error=False`).
#
# 5. Things deliberately missing in s04:
#    - hook callbacks (s09)
#    - sideband injection (`_drain_injections`)
#    - empty-content retries (different hint than length recovery)
#    - micro-compaction (truncate verbose tool outputs mid-run)
#    - checkpoint emission (UI progress streaming)
#    - per-provider retry mode plumbing
#
# Reading map:
#   - nanobot/agent/runner.py:1-100   — imports + dataclasses
#   - nanobot/agent/runner.py:200-300 — the body of run() in full
#   - nanobot/agent/runner.py:300-450 — _handle_length_recovery,
#     _drain_injections, _emit_checkpoint helpers
#   - nanobot/utils/runtime.py        — build_length_recovery_message,
#     build_finalization_retry_message, ensure_nonempty_tool_result
#   - nanobot/agent/loop.py::_state_run — the outer state machine's call
#     site (canonical for learn-nanobot/s11)
#   - nanobot/agent/hook.py — AgentHook + AgentHookContext (canonical
#     for learn-nanobot/s09)
