---
title: "Multi-model integration guide"
slug: multi-model
est_read_min: 12
---

# Multi-model integration guide

> Every one of the 11 sessions ships with both **the native Anthropic protocol** and **OpenAI-compatible** support. The latter covers nearly every mainstream LLM API: DeepSeek, Moonshot/Kimi, Qwen, Groq, OpenRouter, self-hosted vLLM/SGLang, plus OpenAI itself.

## Design: one loop, two wire formats

learn-nanobot's agent loop uses Anthropic-style `Message / ContentBlock` internally (with `tool_use` / `tool_result` blocks). **Translation happens at the provider boundary, exactly once.** s01/s02's `provider_openai.go` translates between OpenAI Chat Completions and Anthropic-flat wire shapes; s03+ translates between OpenAI Chat Completions and the normalized `LLMResponse` (s03's contribution).

```
┌──────────────────────────────────────────────┐
│  Loop / Runner / TurnState (uses LLMResponse) │   ← s03+
│  Loop (uses CreateMessageResponse)            │   ← s01/s02
├─────────────────────┬────────────────────────┤
│ Provider interface  │  Provider interface    │
├─────────────────────┴────────────────────────┤
│   AnthropicProvider │ OpenAIProvider         │
│   /v1/messages      │ /v1/chat/completions   │
│                     │  ├ openai.com          │
│                     │  ├ deepseek.com        │
│                     │  ├ moonshot.cn         │
│                     │  ├ dashscope ...       │
│                     │  └ localhost vLLM      │
└─────────────────────┴────────────────────────┘
```

Each session ships its own `provider_openai.go` (modules are independent). s01/s02's signature is `Provider.CreateMessage`; from s03 onward it's `Provider.Chat → LLMResponse` — same translation idea, different return type.

## 8 provider profiles

Every session's `main.go` exposes this default table via a `-provider` flag:

| `-provider` | endpoint | default model | env var |
|---|---|---|---|
| `anthropic` (default) | api.anthropic.com | claude-sonnet-4-6 | `ANTHROPIC_API_KEY` |
| `openai` | api.openai.com/v1 | gpt-4o-mini | `OPENAI_API_KEY` |
| `deepseek` | api.deepseek.com/v1 | deepseek-chat | `DEEPSEEK_API_KEY` |
| `moonshot` | api.moonshot.cn/v1 | moonshot-v1-8k | `MOONSHOT_API_KEY` |
| `qwen` | dashscope.aliyuncs.com/compatible-mode/v1 | qwen-plus | `DASHSCOPE_API_KEY` |
| `groq` | api.groq.com/openai/v1 | llama-3.3-70b-versatile | `GROQ_API_KEY` |
| `openrouter` | openrouter.ai/api/v1 | openai/gpt-4o-mini | `OPENROUTER_API_KEY` |
| `local` | http://localhost:8000/v1 | local-model | `OPENAI_API_KEY` |

## Practical

```bash
cd agents/s01-minimum-loop

# Default (Anthropic)
export ANTHROPIC_API_KEY=sk-ant-...
go run . "hello"

# DeepSeek
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "list .go files"

# Qwen
export DASHSCOPE_API_KEY=sk-...
go run . -provider qwen "what's the weather like this weekend?"

# Self-hosted vLLM
go run . -provider local -base-url http://127.0.0.1:8000/v1 -model my-llama "hi"

# Any OpenAI-compatible endpoint
go run . -provider openai -base-url https://your-proxy.example/v1 -model gpt-4 "..."
```

Same pattern for every session — `s11-turn-state-machine` accepts the same `-provider` flags.

## Translation rules (Anthropic ↔ OpenAI)

### Request direction (us → OpenAI)

| Anthropic concept | OpenAI equivalent |
|---|---|
| `system: "..."` (field) | One `{role: "system", content: "..."}` message prepended to messages |
| `{role: "user", content: [{type: "text", text: "..."}]}` | `{role: "user", content: "..."}` (text concatenated) |
| `{role: "user", content: [{type: "tool_result", tool_use_id: X, content: Y}]}` | `{role: "tool", tool_call_id: X, content: Y}` (one tool message per tool_result block) |
| `{role: "assistant", content: [{type: "tool_use", id: X, name: N, input: I}]}` | `{role: "assistant", tool_calls: [{id: X, type: "function", function: {name: N, arguments: JSON.stringify(I)}}]}` |
| `tools: [{name: N, input_schema: S}]` | `tools: [{type: "function", function: {name: N, parameters: S}}]` |

### Response direction (OpenAI → us)

| OpenAI field | Our equivalent |
|---|---|
| `choices[0].message.content: "..."` | `{type: "text", text: "..."}` block (s01/s02) or `LLMResponse.Content += "..."` (s03+) |
| `choices[0].message.tool_calls: [...]` | Parse `function.arguments` (JSON string) into `Input map`; emit `tool_use` block or `LLMResponse.ToolCalls = append(...)` |
| `finish_reason: "stop"` | `stop_reason: "end_turn"` (s01/s02) / `FinishReason: "stop"` (s03+) |
| `finish_reason: "tool_calls"` | `stop_reason: "tool_use"` / `FinishReason: "tool_calls"` |
| `finish_reason: "length"` | `stop_reason: "max_tokens"` / `FinishReason: "length"` |
| `usage: {prompt_tokens, completion_tokens}` | `Usage{InputTokens, OutputTokens}` |

Code lives in each session's `provider_openai.go` — `translateRequestToOpenAI` + `translateResponseFromOpenAI`, ~200 LOC each.

## Known gotchas

- **DeepSeek** sometimes returns `content` as `[{type: "text", text: "..."}]` array instead of a string — the translator's `contentToString` handles it.
- **Qwen** defaults `max_tokens` to 6K; overruns silently truncate, so pass `-max-turns` explicitly to let the loop bail.
- **Groq** free tier has RPM limits; the retry-after header isn't always populated.
- **OpenRouter** — some models don't support tool calling (model card "function calling: NO"). Tool-using sessions break with those models.
- **Self-hosted vLLM** needs `--enable-auto-tool-choice` to enable tool calling (SGLang has a similar flag).
- **MiniMax / Zhipu / others** aren't in the 8-profile table — their OpenAI-compatible endpoints work via `-provider openai -base-url <endpoint> -model <model-id>`.

## Adding your own provider profile

Every session already ships `provider_openai.go` + `provider_openai_test.go`. To add a new OpenAI-compatible provider not in the 8-profile list:

```go
// In main.go's providerProfiles map, add:
"my-vendor": {
    BaseURL: "https://api.my-vendor.com/v1",
    Model:   "my-vendor-flagship-model",
    APIKey:  "MY_VENDOR_API_KEY",
},
```

Then `export MY_VENDOR_API_KEY=...; go run . -provider my-vendor "..."`. Zero code changes elsewhere.

For providers that are **not** OpenAI-compatible — say, custom protocols — you write a fresh `Provider` impl alongside `provider_anthropic.go` / `provider_openai.go`. s03's `LLMResponse` abstraction was designed for exactly this case.

## References

- Upstream nanobot's `nanobot/providers/openai_compat_provider.py` — same idea, more edge cases
- s01 + s02 + s03's `provider_openai.go` — diff them to see "what the translator looks like before vs. after the Provider interface change"
