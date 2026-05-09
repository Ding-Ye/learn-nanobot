---
title: "s03 · Provider abstraction"
chapter: 3
slug: s03-provider-abstraction
est_read_min: 22
---

# s03 · Provider abstraction

> What this teaches: hide Anthropic's wire format behind a normalized `LLMResponse`. Any backend slots in by writing one `Provider` impl.

---

## Problem

In s01/s02 `Provider.CreateMessage` returned `CreateMessageResponse` — an Anthropic-specific shape. The Loop unpacked `resp.Content[].Type == "tool_use"` and `resp.StopReason == "end_turn"` itself. The Loop is conceptually bound to Anthropic; switching to OpenAI / Bedrock / DeepSeek means editing `loop.go`.

But upstream nanobot supports 9 providers, and the way it manages that is by stuffing all "how to call this LLM + parse its response" into the Provider layer. The Loop sees only a unified `LLMResponse`. s03 builds that seam — and lets Phase G (multi-model addendum) plug in OpenAI-compat without touching anything else.

## Solution

The `Provider` interface tightens to:

```go
type Provider interface {
    Chat(ctx context.Context, req ChatReq) (LLMResponse, error)
}
```

`ChatReq` is a provider-agnostic input; `LLMResponse` is a provider-agnostic output containing:
- `Content string` — all text blocks concatenated
- `ToolCalls []ToolCall` — already-translated flat tool requests
- `FinishReason string` — normalized to `stop` / `tool_calls` / `length` / `error`
- `Usage` — standardized token counts
- `Err*` — error classification (rate limit / 5xx / quota), with `ErrShouldRetry bool`
- `AssistantBlocks []ContentBlock` — Anthropic-shaped raw blocks for the Loop to append back into history (protocol requirement; non-Anthropic providers synthesize equivalent blocks)

Translation work happens in each concrete Provider's file: `provider_anthropic.go` translates Anthropic's wire shape into LLMResponse; Phase G's `provider_openai.go` will translate OpenAI's wire shape into the same LLMResponse.

Three key decisions:

1. **`AssistantBlocks` must preserve Anthropic shape.** Protocol requires the assistant's tool_use blocks to be appended back to history with their original structure. So even though the LLMResponse has flat `Content` and `ToolCalls`, the Provider also retains the original blocks. Other providers synthesize equivalent blocks at parse time.
2. **`FinishReason` is one of four normalized values.** `stop` / `tool_calls` / `length` / `error`. The Loop switches on this; it never touches provider-specific strings.
3. **Errors come back inside `LLMResponse`** (FinishReason="error"), not as Go errors. This way s04's Runner can read ErrShouldRetry and decide whether to retry — if Chat raised, that information would be lost.

## How It Works

```ascii-anim frames=2
┌───────────────────────────────────────────────────────────┐
│                                                           │
│   Loop ─► provider.Chat(ChatReq{messages, tools, model})  │
│                            │                              │
│                            ▼                              │
│            AnthropicProvider.Chat                          │
│             ├─ marshal ChatReq → anthropicReq             │
│             ├─ POST https://api.anthropic.com/v1/messages │
│             ├─ if 4xx/5xx: classifyRetry → Err* fields    │
│             └─ unmarshal & translate:                     │
│                  - text block  → Content +=               │
│                  - tool_use    → ToolCalls = append(...)  │
│                  - stop_reason → FinishReason mapping     │
│                            │                              │
│                            ▼                              │
│                       LLMResponse                         │
│                            │                              │
│   Loop ◄────────────────── ▼                              │
│      switch resp.FinishReason {                           │
│        case "stop":       return resp.Content             │
│        case "tool_calls": runTools(resp.ToolCalls)        │
│        case "length":     return error                    │
│        case "error":      return wrapped error            │
│      }                                                    │
│                                                           │
└───────────────────────────────────────────────────────────┘
```

The core ~30 lines (excerpt from [`agents/s03-provider-abstraction/provider_anthropic.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s03-provider-abstraction/provider_anthropic.go)):

```go
func (a *AnthropicProvider) Chat(ctx context.Context, req ChatReq) (LLMResponse, error) {
    // ... HTTP POST omitted ...
    if resp.StatusCode/100 != 2 {
        return LLMResponse{
            FinishReason:   "error",
            ErrCode:        fmt.Sprintf("http_%d", resp.StatusCode),
            ErrMessage:     truncateBody(string(respBody)),
            ErrShouldRetry: classifyRetry(resp.StatusCode, respBody),
        }, nil
    }
    var aresp anthropicResp
    json.Unmarshal(respBody, &aresp)

    out := LLMResponse{
        AssistantBlocks: aresp.Content,
        Usage: Usage{InputTokens: aresp.Usage.InputTokens, OutputTokens: aresp.Usage.OutputTokens},
    }
    for _, b := range aresp.Content {
        switch b.Type {
        case "text":
            out.Content += b.Text
        case "tool_use":
            out.ToolCalls = append(out.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Input: b.Input})
        }
    }
    switch aresp.StopReason {
    case "end_turn", "stop_sequence": out.FinishReason = "stop"
    case "tool_use":                  out.FinishReason = "tool_calls"
    case "max_tokens":                out.FinishReason = "length"
    }
    return out, nil
}
```

**4 non-obvious points**:

1. **Errors flow via `LLMResponse`, not Go errors.** `Chat` returns a non-nil Go error only when it can't even construct the request or the network layer panics. All HTTP 4xx/5xx come back as `LLMResponse{FinishReason: "error", ...}`. This is what lets retry info (ErrShouldRetry) propagate from Provider → Runner (s04) → Hook (s09).
2. **`AssistantBlocks` preserves wire shape *verbatim*.** The Loop appends this back to history; the protocol requires tool_use blocks to be present in their original block structure. If the Loop tried to reconstruct blocks from `Content` and `ToolCalls`, it would lose thinking blocks and any future block types.
3. **`classifyRetry` uses substring scan for quota-vs-rate-limit.** It's a slim subset of upstream's `_NON_RETRYABLE_429_ERROR_TOKENS` set — production nanobot has 8 distinct 429 categories; 4 is enough for teaching.
4. **`FakeProvider` is the test backbone.** It's just a slice of LLMResponses. Every later session's LLM-related tests use FakeProvider — no API key required.

## What Changed (vs. s02)

```diff
  type Provider interface {
-     CreateMessage(ctx context.Context, req CreateMessageRequest) (*CreateMessageResponse, error)
+     Chat(ctx context.Context, req ChatReq) (LLMResponse, error)
  }

+ type LLMResponse struct {
+     Content         string
+     ToolCalls       []ToolCall
+     AssistantBlocks []ContentBlock
+     FinishReason    string
+     Usage           Usage
+     ErrCode         string
+     ErrMessage      string
+     ErrShouldRetry  bool
+ }
+ type ToolCall struct {
+     ID    string
+     Name  string
+     Input map[string]interface{}
+ }

  // Loop body — switch on FinishReason, not StopReason:
  switch resp.FinishReason {
- case "end_turn", "stop_sequence":  return extractText(resp.Content), nil
+ case "stop":                       return resp.Content, nil
- case "tool_use":
-     results := runTools(resp.Content, ...)
+ case "tool_calls":
+     results := runToolCalls(resp.ToolCalls, ...)
- case "max_tokens":                 return error
+ case "length":                     return error
+ case "error":                      return wrapped(ErrShouldRetry, ErrMessage)
  }
```

`AnthropicProvider` got heavier (it absorbed the wire-format parsing the Loop used to do). The Loop is *thinner* — it doesn't say the word "Anthropic" anymore.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s03-provider-abstraction

go run . -v "list .go files"

# Tests cover retry classification, FakeProvider, and Provider error wrapping.
go test -v ./...

# Trigger a deliberate auth error (401) to see the LLMResponse error shape.
ANTHROPIC_API_KEY=invalid go run . "test" 2>&1 | head -5
```

Expected output shape:

```
$ go run . -v "list .go files"
[s03] model=claude-sonnet-4-6 tools=1 max_turns=20
[turn 0] assistant: I'll list the .go files.
[turn 0] -> bash map[command:ls *.go]
[turn 0] <- bash_tool.go loop.go main.go provider.go provider_anthropic.go provider_fake.go registry.go tools.go
[turn 1] assistant: Eight .go files: ...
```

## Upstream Source Reading

Upstream's `nanobot/providers/base.py` defines the `LLMProvider` ABC + `LLMResponse` dataclass + `ToolCallRequest` dataclass. Full annotated excerpt at [`upstream-readings/s03-provider-abstraction.py`](../../upstream-readings/s03-provider-abstraction.py).

```upstream:nanobot/providers/base.py#L40-L100
@dataclass
class LLMResponse:
    """Response from an LLM provider."""
    content: str | None
    tool_calls: list[ToolCallRequest] = field(default_factory=list)
    finish_reason: str = "stop"
    usage: dict[str, int] = field(default_factory=dict)
    retry_after: float | None = None
    reasoning_content: str | None = None
    thinking_blocks: list[dict] | None = None
    error_status_code: int | None = None
    error_kind: str | None = None
    error_type: str | None = None
    error_code: str | None = None
    error_should_retry: bool | None = None

    @property
    def has_tool_calls(self) -> bool:
        return len(self.tool_calls) > 0

    @property
    def should_execute_tools(self) -> bool:
        if not self.has_tool_calls:
            return False
        return self.finish_reason in ("tool_calls", "stop")
```

**Reading notes**:

- **`thinking_blocks` we don't surface** — Anthropic's chain-of-thought is in `<thinking>...</thinking>` blocks; upstream can forward these to a Hook. s09 teaches Hooks; thinking forwarding is an extension exercise.
- **`should_execute_tools` blocks injected tool_calls under refusal/content_filter** — production safety (issue #3220 in nanobot's history). s03 simplifies to "FinishReason == 'tool_calls'". Worth reading for production hardening.
- **Upstream's `LLMProvider.chat` has built-in retry policy** (`_CHAT_RETRY_DELAYS = (1, 2, 4)` etc.). We don't do that in s03 — s04's Runner takes that responsibility, so Provider stays "call once, return LLMResponse".
- **`to_openai_tool_call`** is the Phase G hook — the same `ToolCallRequest` serializes into OpenAI's `{"id": ..., "type": "function", "function": {"name": ..., "arguments": <json-string>}}` shape via a pure-function method on the dataclass.
- **`error_kind` vs `error_should_retry`** — upstream distinguishes `timeout` / `connection` / `rate_limit` / `quota`; we collapse into `error_should_retry: bool`. Production projects with detailed observability needs should split these.

**Read further**:
1. `nanobot/providers/base.py` — the full `LLMProvider` definition, especially `_TRANSIENT_ERROR_MARKERS` and `_NON_RETRYABLE_429_ERROR_TOKENS`.
2. `nanobot/providers/anthropic_provider.py` — `_build_kwargs` (request translation) and `_parse_response` (response translation), the Python mirrors of our `provider_anthropic.go`.
3. `nanobot/providers/openai_compat_provider.py` — Phase G's reference for what an OpenAI-compatible impl looks like.

---

**Next**: s04 carves the "call LLM ↔ execute tools" core out of the Loop into a `Runner`, preparing s11 to wrap it in an outer state machine. The name `Loop` gets parked until s11 reuses it.
