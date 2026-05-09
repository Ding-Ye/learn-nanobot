---
title: "多模型接入指南"
slug: multi-model
est_read_min: 12
---

# 多模型接入指南

> 11 个 session 都内置 **Anthropic 原生协议** + **OpenAI-compatible 协议** 双线接入。后者覆盖了几乎所有主流大模型 API：DeepSeek、Moonshot/Kimi、Qwen、Groq、OpenRouter、自托管 vLLM/SGLang，以及 OpenAI 自己。

## 设计：一份 loop，两套 wire format

learn-nanobot 的 agent loop 内部用 Anthropic 风格的 `Message / ContentBlock`（含 `tool_use` / `tool_result` 块）。**翻译只在 provider 边界做一次**——s01/s02 在 `provider_openai.go` 里把 OpenAI Chat Completions 的 wire 翻成 Anthropic-flat；s03+ 把 OpenAI wire 翻成规范化的 `LLMResponse`（s03 教过的）。

```
┌──────────────────────────────────────────────┐
│  Loop / Runner / TurnState (用 LLMResponse)   │   ← s03+
│  Loop (用 CreateMessageResponse)              │   ← s01/s02
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

每一节自带 `provider_openai.go` 文件，独立 module 互不引用。s01/s02 的实现签名是 `Provider.CreateMessage`；s03 之后是 `Provider.Chat → LLMResponse`——同一套翻译思路，只是返回类型不同。

## 8 个 provider profile

每个 session 的 `main.go` 都把这套 profile 当作 `-provider` flag 默认表：

| `-provider` | endpoint | 默认 model | env var |
|---|---|---|---|
| `anthropic` (默认) | api.anthropic.com | claude-sonnet-4-6 | `ANTHROPIC_API_KEY` |
| `openai` | api.openai.com/v1 | gpt-4o-mini | `OPENAI_API_KEY` |
| `deepseek` | api.deepseek.com/v1 | deepseek-chat | `DEEPSEEK_API_KEY` |
| `moonshot` | api.moonshot.cn/v1 | moonshot-v1-8k | `MOONSHOT_API_KEY` |
| `qwen` | dashscope.aliyuncs.com/compatible-mode/v1 | qwen-plus | `DASHSCOPE_API_KEY` |
| `groq` | api.groq.com/openai/v1 | llama-3.3-70b-versatile | `GROQ_API_KEY` |
| `openrouter` | openrouter.ai/api/v1 | openai/gpt-4o-mini | `OPENROUTER_API_KEY` |
| `local` | http://localhost:8000/v1 | local-model | `OPENAI_API_KEY` |

## 实战

```bash
cd agents/s01-minimum-loop

# 默认 (Anthropic)
export ANTHROPIC_API_KEY=sk-ant-...
go run . "hello"

# DeepSeek
export DEEPSEEK_API_KEY=sk-...
go run . -provider deepseek -v "list .go files"

# Qwen / 通义千问
export DASHSCOPE_API_KEY=sk-...
go run . -provider qwen "周末天气如何？"

# 自托管 vLLM
go run . -provider local -base-url http://127.0.0.1:8000/v1 -model my-llama "hi"

# 任意 OpenAI-compatible 端点
go run . -provider openai -base-url https://your-proxy.example/v1 -model gpt-4 "..."
```

每个 session 跑法相同——`s11-turn-state-machine` 也是 `go run . -provider deepseek "..."`。

## 翻译规则（Anthropic ↔ OpenAI）

### 请求方向（我们 → OpenAI）

| Anthropic 概念 | OpenAI 等价 |
|---|---|
| `system: "..."` (字段) | 一条 `{role: "system", content: "..."}` 消息塞进 messages 头部 |
| `{role: "user", content: [{type: "text", text: "..."}]}` | `{role: "user", content: "..."}` (text 拼接) |
| `{role: "user", content: [{type: "tool_result", tool_use_id: X, content: Y}]}` | `{role: "tool", tool_call_id: X, content: Y}` (一个 tool_result block 一条 tool message) |
| `{role: "assistant", content: [{type: "tool_use", id: X, name: N, input: I}]}` | `{role: "assistant", tool_calls: [{id: X, type: "function", function: {name: N, arguments: JSON.stringify(I)}}]}` |
| `tools: [{name: N, input_schema: S}]` | `tools: [{type: "function", function: {name: N, parameters: S}}]` |

### 响应方向（OpenAI → 我们）

| OpenAI 字段 | 我们的等价 |
|---|---|
| `choices[0].message.content: "..."` | `{type: "text", text: "..."}` block (s01/s02) 或 `LLMResponse.Content += "..."` (s03+) |
| `choices[0].message.tool_calls: [...]` | 解析 `function.arguments` (JSON string) 成 `Input map`；产生 `tool_use` block 或 `LLMResponse.ToolCalls = append(...)` |
| `finish_reason: "stop"` | `stop_reason: "end_turn"` (s01/s02) / `FinishReason: "stop"` (s03+) |
| `finish_reason: "tool_calls"` | `stop_reason: "tool_use"` / `FinishReason: "tool_calls"` |
| `finish_reason: "length"` | `stop_reason: "max_tokens"` / `FinishReason: "length"` |
| `usage: {prompt_tokens, completion_tokens}` | `Usage{InputTokens, OutputTokens}` |

代码全在 `provider_openai.go` 的 `translateRequestToOpenAI` 和 `translateResponseFromOpenAI`。每个 session 里都是这 ~200 行。

## 已知坑

- **DeepSeek** 偶尔返回 `content` 是数组 `[{type: "text", text: "..."}]` 而非 string——翻译层用 `contentToString` 兜底
- **Qwen** `max_tokens` 默认 6K；超出会 silent truncate，建议显式传 `-max-turns` 让 loop 退出
- **Groq** 免费额度有 RPM 限制；hit 之后 retry-after 不规范
- **OpenRouter** 部分模型不支持 tool calling（model card 标 `function calling: NO`）；agent loop 在 Tool 工作时这些模型不能用
- **本地 vLLM** 想用 tool calling 需要 `--enable-auto-tool-choice` 启动（SGLang 类似）
- **GBT/MiniMax/智谱** 等没在 8-profile 里——它们的 OpenAI-compatible 端点可以用 `-provider openai -base-url <their-endpoint> -model <their-model>` 跑

## 给其它 session 加自己的 provider

每个 session 已经有 `provider_openai.go` + `provider_openai_test.go`。如果你想加一个 8-profile 之外的 provider：

```go
// 在 main.go 的 providerProfiles map 里加一行
"my-vendor": {
    BaseURL: "https://api.my-vendor.com/v1",
    Model:   "my-vendor-flagship-model",
    APIKey:  "MY_VENDOR_API_KEY",
},
```

然后 `export MY_VENDOR_API_KEY=...; go run . -provider my-vendor "..."`。零代码改动。

如果该 provider **不是 OpenAI-compatible**——譬如它有完全自定义的协议——那就得仿照 `provider_anthropic.go` 写一个新的 `Provider` impl。s03 教的 `LLMResponse` 抽象就是为这种情况准备的。

## 参考链接

- 上游 nanobot 的 `nanobot/providers/openai_compat_provider.py` — 同样的设计，更多 edge case 处理
- s01 + s02 + s03 的 `provider_openai.go` —— 可以直接 diff 看清"Provider 接口换了之后翻译层怎么改"
