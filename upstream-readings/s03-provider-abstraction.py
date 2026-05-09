# Source: HKUDS/nanobot · nanobot/providers/base.py + nanobot/providers/anthropic_provider.py
# License: MIT
# Fetched: 2026-05-09 from main branch
#
# Teaching excerpt for learn-nanobot/s03-provider-abstraction.
# Upstream provider/base.py is ~600 lines because it carries 8 levels of
# retry policy. This excerpt keeps the dataclasses + the abstract method
# only — the conceptual core that learn-nanobot/s03 reproduces.

# ────────────────────────────────────────────────────────────────────
# Part 1. nanobot/providers/base.py — ToolCallRequest + LLMResponse
# ────────────────────────────────────────────────────────────────────

@dataclass
class ToolCallRequest:
    """A tool call request from the LLM."""
    id: str
    name: str
    arguments: dict[str, Any]
    extra_content: dict[str, Any] | None = None
    provider_specific_fields: dict[str, Any] | None = None

    def to_openai_tool_call(self) -> dict[str, Any]:
        """Serialize to an OpenAI-style tool_call payload.

        Phase G of learn-nanobot uses this exact shape — converting between
        Anthropic's flat tool_use blocks and OpenAI's nested function-call
        JSON is a pure-function task on the dataclass.
        """
        ...


@dataclass
class LLMResponse:
    """Response from an LLM provider — provider-agnostic."""
    content: str | None
    tool_calls: list[ToolCallRequest] = field(default_factory=list)
    finish_reason: str = "stop"
    usage: dict[str, int] = field(default_factory=dict)

    # Reasoning / thinking — Anthropic streams these as "thinking" blocks;
    # learn-nanobot doesn't expose them yet (extension exercise).
    retry_after: float | None = None
    reasoning_content: str | None = None
    thinking_blocks: list[dict] | None = None

    # Error fields — populated when the provider response is itself an error
    # (rate limit, 5xx, billing). Match learn-nanobot/s03's Err* fields.
    error_status_code: int | None = None
    error_kind: str | None = None
    error_type: str | None = None
    error_code: str | None = None
    error_retry_after_s: float | None = None
    error_should_retry: bool | None = None

    @property
    def has_tool_calls(self) -> bool:
        return len(self.tool_calls) > 0

    @property
    def should_execute_tools(self) -> bool:
        """Tools execute only when has_tool_calls AND finish_reason in
        {'tool_calls','stop'}. Refusal/content-filter errors block.
        """
        if not self.has_tool_calls:
            return False
        return self.finish_reason in ("tool_calls", "stop")


class LLMProvider(ABC):
    """Base class for LLM providers."""

    # Built-in retry policy — learn-nanobot doesn't implement these yet;
    # s04's Runner will pick up the responsibility (centralized retry).
    _CHAT_RETRY_DELAYS = (1, 2, 4)
    _PERSISTENT_MAX_DELAY = 60

    # Heuristic table for retry classification. learn-nanobot/s03's
    # classifyRetry() implements a slim version of this.
    _RETRYABLE_STATUS_CODES = frozenset({408, 409, 429})
    _NON_RETRYABLE_429_ERROR_TOKENS = frozenset({
        "insufficient_quota", "quota_exceeded", "billing_hard_limit_reached",
        "insufficient_balance", "credit_balance_too_low", "billing_not_active",
        "payment_required",
    })
    _RETRYABLE_429_ERROR_TOKENS = frozenset({
        "rate_limit_exceeded", "rate_limit_error", "too_many_requests",
        "request_limit_exceeded", "overloaded_error",
    })

    @abstractmethod
    async def chat(
        self,
        messages: list[dict[str, Any]],
        tools: list[dict[str, Any]] | None = None,
        model: str | None = None,
        max_tokens: int = 4096,
        temperature: float = 0.7,
        reasoning_effort: str | None = None,
        tool_choice: str | dict[str, Any] | None = None,
    ) -> LLMResponse:
        """Send a chat completion request. Must return LLMResponse."""


# ────────────────────────────────────────────────────────────────────
# Part 2. nanobot/providers/anthropic_provider.py — translator (sketch)
# ────────────────────────────────────────────────────────────────────

class AnthropicProvider(LLMProvider):
    async def chat(self, messages, tools, model, max_tokens, temperature, ...):
        # 1. Translate the request: messages stay as-is (Anthropic's wire
        #    format already matches what we accept), tools stay as-is.
        kwargs = self._build_kwargs(messages, tools, model, max_tokens, temperature)

        # 2. Make the call (uses anthropic SDK, supports streaming).
        try:
            response = await self._client.messages.create(**kwargs)
        except RateLimitError as e:
            return self._classify_429_error(e)
        except APIConnectionError as e:
            return LLMResponse(content=None, finish_reason="error",
                               error_kind="connection", error_should_retry=True,
                               error_message=str(e))
        except Exception as e:
            return LLMResponse(content=None, finish_reason="error",
                               error_kind="permanent", error_should_retry=False,
                               error_message=str(e))

        # 3. Translate response: walk content blocks, extract text into
        #    `content` and tool_use blocks into `tool_calls`. Map
        #    stop_reason → finish_reason.
        return self._parse_response(response)

    def _parse_response(self, raw) -> LLMResponse:
        text_parts = []
        tool_calls = []
        for block in raw.content:
            if block.type == "text":
                text_parts.append(block.text)
            elif block.type == "tool_use":
                tool_calls.append(ToolCallRequest(
                    id=block.id, name=block.name, arguments=block.input,
                ))
            # Anthropic also emits "thinking" blocks; we drop them in this
            # excerpt for clarity (learn-nanobot doesn't surface them yet).

        finish = {
            "end_turn": "stop",
            "stop_sequence": "stop",
            "tool_use": "tool_calls",
            "max_tokens": "length",
        }.get(raw.stop_reason, raw.stop_reason)

        return LLMResponse(
            content="".join(text_parts),
            tool_calls=tool_calls,
            finish_reason=finish,
            usage={"input_tokens": raw.usage.input_tokens,
                   "output_tokens": raw.usage.output_tokens},
        )


# ────────────────────────────────────────────────────────────────────
# What this excerpt teaches (vs. learn-nanobot/s03)
# ────────────────────────────────────────────────────────────────────
#
# 1. Same shape: LLMResponse has content + tool_calls + finish_reason +
#    usage + error fields. learn-nanobot/s03/provider.go matches this.
#
# 2. Translation lives entirely in the concrete Provider. The base class
#    knows nothing about Anthropic blocks or OpenAI function-call JSON;
#    it only knows the normalized LLMResponse.
#
# 3. Upstream's retry policy is built into LLMProvider.chat (delays, max
#    persistent error count, heartbeats). learn-nanobot's s03 only
#    classifies retries — actual retry logic is centralized in s04 (Runner)
#    so we don't tangle "what a Provider is" with "how to retry it".
#
# 4. Upstream supports `thinking_blocks` for chain-of-thought visibility;
#    learn-nanobot drops them (extension exercise: forward to a Hook).
#
# 5. `should_execute_tools` is upstream's defense against gateway-injected
#    tool calls under refusal/content_filter — see issue #3220 in nanobot's
#    history. learn-nanobot doesn't reproduce this; it's a production-only
#    concern when proxy-injected tool calls are possible.
#
# Reading map:
#   - nanobot/providers/base.py — start at LLMResponse, then `chat` ABC
#   - nanobot/providers/anthropic_provider.py — _build_kwargs (request
#     translation) and _parse_response (response translation)
#   - nanobot/providers/openai_compat_provider.py — Phase G's reference
#     for what an OpenAI-style impl looks like (it has the same shape but
#     translates `function.arguments` JSON-string into ToolCallRequest)
#   - nanobot/agent/runner.py — sees `provider.chat(...)` as the only
#     call site; it's the intended consumer of LLMResponse
