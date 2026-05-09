# Source: HKUDS/nanobot · nanobot/agent/loop.py + nanobot/agent/runner.py
# License: MIT (https://github.com/HKUDS/nanobot/blob/main/LICENSE)
# Fetched: 2026-05-09 from main branch
#
# Teaching excerpt for learn-nanobot/s01-minimum-loop.
# Simplified: comments translated/expanded; imports trimmed; error-handling
# flattened. The full files are 600+ and 800+ lines; this excerpt captures
# the conceptual core that s01 re-implements.
#
# Reading order:
#   1. AgentLoop's import surface (loop.py:1-25) — the *cast* of characters.
#   2. AgentRunSpec / AgentRunResult (runner.py:55-95) — what flows in/out.
#   3. The conceptual "for turn in range(max_iterations)" core (runner.py).

# ─────────────────────────────────────────────────────────────────────
# Part 1. nanobot/agent/loop.py (lines 1-25, the imports tell the story)
# ─────────────────────────────────────────────────────────────────────
"""Agent loop: the core processing engine."""

from __future__ import annotations

import asyncio
import dataclasses
from contextlib import AsyncExitStack
from dataclasses import dataclass, field
from enum import Enum, auto
from pathlib import Path
from typing import Any

# THE CAST OF CHARACTERS — what AgentLoop wires together.
# In learn-nanobot, each of these is the subject of one chapter:
from nanobot.agent.autocompact import AutoCompact          # learn-nanobot s10
from nanobot.agent.context import ContextBuilder            # learn-nanobot s08
from nanobot.agent.hook import AgentHook, CompositeHook     # learn-nanobot s09
from nanobot.agent.memory import Consolidator               # learn-nanobot s10
from nanobot.agent.runner import AgentRunner, AgentRunSpec  # learn-nanobot s04
from nanobot.agent.tools.registry import ToolRegistry       # learn-nanobot s02
from nanobot.bus.queue import MessageBus                    # learn-nanobot s05
from nanobot.providers.base import LLMProvider              # learn-nanobot s03
from nanobot.session.manager import Session, SessionManager # learn-nanobot s05
# (...plus 30+ more imports for tools, file-state, web-search, image-gen, etc.)

# Reading note for s01: just look at the imports above. Each "from nanobot.X
# import Y" line is one mechanism — and learn-nanobot's curriculum is
# basically "build each of these in order". s01 is the smallest agent that
# uses *none* of them: just an LLM client + 1 tool + a for-loop.

# ─────────────────────────────────────────────────────────────────────
# Part 2. nanobot/agent/runner.py — RunSpec / RunResult (lines 55-95)
# ─────────────────────────────────────────────────────────────────────

@dataclass(slots=True)
class AgentRunSpec:
    """Configuration for a single agent execution."""

    initial_messages: list[dict[str, Any]]   # the conversation so far
    tools: ToolRegistry                       # what the agent can call
    model: str                                # which LLM to talk to
    max_iterations: int                       # === our `MaxTurns`
    max_tool_result_chars: int                # truncate tool output
    temperature: float | None = None
    max_tokens: int | None = None
    hook: AgentHook | None = None             # learn-nanobot s09
    fail_on_tool_error: bool = False
    workspace: Path | None = None
    session_key: str | None = None
    # ... 10+ more fields for streaming, injections, retry policy, etc.


@dataclass(slots=True)
class AgentRunResult:
    """Outcome of a shared agent execution."""

    final_content: str | None                 # what we return to the caller
    messages: list[dict[str, Any]]            # full conversation including tool turns
    tools_used: list[str] = field(default_factory=list)
    usage: dict[str, int] = field(default_factory=dict)
    stop_reason: str = "completed"
    error: str | None = None

# Reading note for s01: our Loop.Run returns `(string, error)` directly.
# Upstream returns this much richer struct because it has streaming,
# retries, telemetry, tool-events. We don't have any of that yet.

# ─────────────────────────────────────────────────────────────────────
# Part 3. nanobot/agent/runner.py — the conceptual core of run()
#         (heavily simplified; real implementation is ~250 lines)
# ─────────────────────────────────────────────────────────────────────

class AgentRunner:
    """Run a tool-capable LLM loop without product-layer concerns."""

    def __init__(self, provider: LLMProvider):
        self.provider = provider

    async def run(self, spec: AgentRunSpec) -> AgentRunResult:
        # 1. Bootstrap: messages start as whatever the caller passed in.
        messages = list(spec.initial_messages)
        tools_used: list[str] = []

        # 2. The for-loop. This is the ENTIRE "agent" idea.
        for iteration in range(spec.max_iterations):
            # 2a. Call the LLM.
            #   In our s01: provider.CreateMessage(req) → CreateMessageResponse
            #   Upstream:  provider.chat(messages, tools, model, …) → LLMResponse
            response: LLMResponse = await self.provider.chat(
                messages=messages,
                tools=spec.tools.get_definitions(),  # ← s02 teaches this
                model=spec.model,
                temperature=spec.temperature,
                max_tokens=spec.max_tokens,
            )

            # 2b. Append the assistant turn — even if it's all tool_use
            # blocks, the protocol requires the assistant message to live
            # in history. (Our s01 does exactly this.)
            messages.append({
                "role": "assistant",
                "content": response.content_blocks,
            })

            # 2c. Decide what to do based on stop_reason / finish_reason.
            #
            # Upstream classifies into many flavors (length recovery,
            # blank content retry, tool errors); s01 handles only the
            # three obvious ones: end_turn, tool_use, max_tokens.
            if not response.tool_calls:
                # No tools requested → the model is "done talking".
                return AgentRunResult(
                    final_content=response.text,
                    messages=messages,
                    tools_used=tools_used,
                )

            # 2d. Execute tool calls. Upstream: optionally concurrent,
            # optionally with hooks fired before/after each. s01: serial,
            # no hooks.
            tool_results = []
            for call in response.tool_calls:
                tool = spec.tools.get(call.name)
                if tool is None:
                    # Unknown tool → record an error tool_result, keep going.
                    # Same logic as our s01.
                    tool_results.append(_unknown_tool_result(call))
                    continue
                try:
                    out = await tool.execute(call.arguments)
                except Exception as e:
                    out = f"tool error: {e}"
                tools_used.append(call.name)
                tool_results.append(_tool_result_block(call.id, out))

            # 2e. Send results back as a *user* message. The Anthropic
            # protocol mandates this: tool_result blocks live in user
            # messages. (Our s01 does the same thing.)
            messages.append({"role": "user", "content": tool_results})

        # 3. Hit max_iterations → upstream raises a typed error and
        # serializes a "max iterations" message; s01 just returns an error.
        return AgentRunResult(
            final_content=None,
            messages=messages,
            stop_reason="max_iterations",
            error=f"hit max_iterations={spec.max_iterations}",
        )


# ─────────────────────────────────────────────────────────────────────
# What this excerpt teaches (vs. our learn-nanobot s01)
# ─────────────────────────────────────────────────────────────────────
#
# 1. The shape is the same. "for iteration in range(max_iterations): call LLM,
#    maybe execute tools, append messages, stop when no tools." Our s01's
#    Loop.Run is this exact pattern in 50 lines of Go.
#
# 2. The differences are all production concerns layered on top:
#      - streaming (s09 hooks)
#      - retries (s03 LLMResponse.error_should_retry)
#      - injections (mid-turn user messages from a UI)
#      - tool-result chars truncation (workspace boundaries)
#      - multi-tool concurrency
#      - persistence (s06 messages flow to history.jsonl)
#    None of this changes the agent IDEA — they're operational scaffolding.
#
# 3. AgentLoop (loop.py) is one level UP from AgentRunner (runner.py).
#    learn-nanobot teaches them in opposite order from how upstream defines
#    them: s01 starts with the runner-shaped loop (just `Loop`), then s04
#    carves out the runner explicitly, then s11 re-introduces a different
#    `Loop` that's the upstream's outer state machine.
#
# 4. We translate `asyncio` → goroutines and `await provider.chat()` →
#    `provider.CreateMessage(ctx, …)` synchronously. The Python loop is
#    cooperative; the Go loop is the same idea but the concurrency model
#    only shows up in s05 (per-session goroutines).
#
# Reading map:
#   - Start at nanobot/agent/runner.py::AgentRunner.run
#   - Follow `provider.chat` into nanobot/providers/base.py (= s03)
#   - Follow `spec.tools.get` into nanobot/agent/tools/registry.py (= s02)
#   - When you see `hook.before_iteration`, jump to nanobot/agent/hook.py (= s09)
#   - Outer loop (the state machine) lives in nanobot/agent/loop.py (= s11)
