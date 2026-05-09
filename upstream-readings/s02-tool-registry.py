# Source: HKUDS/nanobot · nanobot/agent/tools/registry.py
# License: MIT
# Fetched: 2026-05-09 from main branch
#
# Teaching excerpt for learn-nanobot/s02-tool-registry.
# This file is short (~120 lines upstream); we keep it nearly verbatim because
# every line is doing something pedagogically important.

"""Tool registry for dynamic tool management."""

from typing import Any
from nanobot.agent.tools.base import Tool


class ToolRegistry:
    """
    Registry for agent tools.

    Allows dynamic registration and execution of tools.
    """

    def __init__(self):
        self._tools: dict[str, Tool] = {}
        # Cache of get_definitions() output. Invalidated whenever tools are
        # added/removed so the LLM prompt prefix stays stable for cache hits.
        self._cached_definitions: list[dict[str, Any]] | None = None

    def register(self, tool: Tool) -> None:
        self._tools[tool.name] = tool
        self._cached_definitions = None  # invalidate

    def unregister(self, name: str) -> None:
        self._tools.pop(name, None)
        self._cached_definitions = None  # invalidate

    def get(self, name: str) -> Tool | None:
        return self._tools.get(name)

    def has(self, name: str) -> bool:
        return name in self._tools

    @staticmethod
    def _schema_name(schema: dict[str, Any]) -> str:
        """Extract a normalized tool name from either OpenAI- or flat-shaped schemas.

        Upstream supports both shapes because it has multiple providers (s03);
        learn-nanobot's s02 only sees flat schemas because s02 is still using
        s01's Anthropic-native Provider.
        """
        fn = schema.get("function")
        if isinstance(fn, dict):
            name = fn.get("name")
            if isinstance(name, str):
                return name
        name = schema.get("name")
        return name if isinstance(name, str) else ""

    def get_definitions(self) -> list[dict[str, Any]]:
        """Get tool definitions with stable ordering for cache-friendly prompts.

        Built-in tools are sorted first as a stable prefix, then MCP tools are
        sorted and appended. Cached until the next register/unregister.

        WHY THIS MATTERS: Anthropic's prompt cache hashes the prompt prefix.
        If the tool list shifts on every turn (e.g. dict-iteration order in
        Python is *insertion* order — not sort), the prefix hash changes and
        the cache misses. Same logic applies to OpenAI's "prompt caching" and
        any other inference-time KV-cache reuse system.
        """
        if self._cached_definitions is not None:
            return self._cached_definitions

        definitions = [tool.to_schema() for tool in self._tools.values()]
        builtins: list[dict[str, Any]] = []
        mcp_tools: list[dict[str, Any]] = []
        for schema in definitions:
            name = self._schema_name(schema)
            if name.startswith("mcp_"):
                mcp_tools.append(schema)
            else:
                builtins.append(schema)

        builtins.sort(key=self._schema_name)
        mcp_tools.sort(key=self._schema_name)
        self._cached_definitions = builtins + mcp_tools
        return self._cached_definitions

    def prepare_call(
        self, name: str, params: dict[str, Any],
    ) -> tuple[Tool | None, dict[str, Any], str | None]:
        """Resolve, cast, and validate one tool call. Upstream only.

        learn-nanobot/s02 doesn't ship param validation — that arrives later
        when we need typed tool arguments. Upstream's `cast_params` and
        `validate_params` are JSON-Schema based; teach those in an extension
        exercise rather than in the main curriculum.
        """
        # ... (50 LOC of validation; trimmed for teaching) ...

    async def execute(self, name: str, params: dict[str, Any]) -> Any:
        """Execute a tool by name with given parameters."""
        _HINT = "\n\n[Analyze the error above and try a different approach.]"
        tool, params, error = self.prepare_call(name, params)
        if error:
            return error + _HINT  # ← injects a hint to help the model recover

        try:
            assert tool is not None
            result = await tool.execute(**params)
            if isinstance(result, str) and result.startswith("Error"):
                return result + _HINT
            return result
        except Exception as e:
            return f"Error executing {name}: {str(e)}" + _HINT

    @property
    def tool_names(self) -> list[str]:
        return list(self._tools.keys())


# ─────────────────────────────────────────────────────────────────────
# What this excerpt teaches (vs. our learn-nanobot s02)
# ─────────────────────────────────────────────────────────────────────
#
# 1. Same shape: dict[name → tool] + cached definitions list. Invalidate
#    cache on any mutation. Sort builtins-first then MCP-second within each
#    group. learn-nanobot s02 has the exact same logic in registry.go.
#
# 2. Upstream supports both Anthropic-flat and OpenAI-nested schemas via
#    `_schema_name()`. We don't yet — s03 introduces the abstraction.
#
# 3. Param validation: upstream casts and validates against a JSON-Schema
#    fragment per tool (see nanobot/agent/tools/base.py:Schema). learn-nanobot
#    skips this in s02; trust the LLM's tool calls and surface errors as
#    tool_result content. Real systems should validate (extension exercise).
#
# 4. The `_HINT` suffix on errors. Upstream appends "[Analyze the error above
#    and try a different approach.]" to every error tool_result. This is
#    prompt-engineering for tool-recovery — it primes the model to NOT
#    repeat the same call. learn-nanobot's s02 omits this for clarity but
#    it's a practical touch worth stealing in production.
#
# 5. `prepare_call` is the seam where upstream rejects bad calls *before*
#    execute. This is a surface area s02 doesn't have but should grow when
#    we add tools whose execution has side effects.
#
# Reading map:
#   - Start: nanobot/agent/tools/registry.py (this file)
#   - Then: nanobot/agent/tools/base.py — Tool ABC and Schema types
#   - Then: nanobot/agent/tools/filesystem.py — concrete tools using cast/validate
#   - Then: nanobot/agent/runner.py::AgentRunner.run — how the runner consumes
#     `registry.get_definitions()` and `registry.execute()`
