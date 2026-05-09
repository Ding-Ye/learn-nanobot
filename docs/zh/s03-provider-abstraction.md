---
title: "s03 · Provider 抽象层"
chapter: 3
slug: s03-provider-abstraction
est_read_min: 22
---

# s03 · Provider 抽象层

> 教什么：把 Anthropic 的 wire 格式从 Loop 里隔离出去。`Provider` 现在返回规范化的 `LLMResponse`——任何后端写一个 impl 就能接入。

---

## Problem / 问题

s01/s02 里 `Provider.CreateMessage` 直接返回 `CreateMessageResponse`——一个 Anthropic 专属的结构。Loop 自己解析 `resp.Content[].Type == "tool_use"`、`resp.StopReason == "end_turn"`。这意味着 Loop 在概念上是绑定 Anthropic 的，换 OpenAI / Bedrock / DeepSeek 都要改 loop.go。

可上游 nanobot 支持 9 个 provider，关键在于它把"怎么调 LLM"和"怎么解析结果"封装在 Provider 层，loop 只看见统一的 `LLMResponse`。我们 s03 做这个事——为 Phase G（多模型 addendum）开口子。

## Solution / 解决方案

`Provider` 接口收紧成：

```go
type Provider interface {
    Chat(ctx context.Context, req ChatReq) (LLMResponse, error)
}
```

`ChatReq` 是 provider-agnostic 的输入；`LLMResponse` 是 provider-agnostic 的输出，含：
- `Content string` —— 所有 text block 拼起来
- `ToolCalls []ToolCall` —— 已经从 wire 格式翻好的扁平列表
- `FinishReason string` —— 规范化为 `stop` / `tool_calls` / `length` / `error`
- `Usage` —— 标准的 token 统计
- `Err*` —— 错误响应（rate limit / 5xx / quota）的分类，含 `ErrShouldRetry bool`
- `AssistantBlocks []ContentBlock` —— Anthropic 形态的原始 blocks，让 Loop 把它 append 回 history（协议要求；非 Anthropic provider 自己合成等价 blocks）

具体 provider 的"翻译"工作只发生在它自己的文件里：`provider_anthropic.go` 把 Anthropic wire 翻成 LLMResponse；Phase G 的 `provider_openai.go` 把 OpenAI wire 翻成同样的 LLMResponse。

三个关键决策：

1. **`AssistantBlocks` 必须保留 Anthropic 形态。** 因为 protocol 要求 assistant 的 tool_use blocks 必须原样回传到 history，所以 Provider 翻译也要负责把 LLMResponse 反翻回 ContentBlock 形态。OpenAI 等其它 provider 在 _parse_response 时合成等价 blocks。
2. **`FinishReason` 只有 4 种规范值。** `stop` / `tool_calls` / `length` / `error`。Loop 用这个 switch，不再触碰 provider-specific 字符串。
3. **错误也通过 `LLMResponse` 返回**（FinishReason="error"），不是 Go error。这样 s04 的 Runner 才能看 ErrShouldRetry 决定是否 retry——如果错误从 Chat 直接抛出去，retry 信息就丢了。

## How It Works / 工作原理

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

核心 ~30 行（节选自 [`agents/s03-provider-abstraction/provider_anthropic.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s03-provider-abstraction/provider_anthropic.go)）：

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

**4 个非显然之处**：

1. **错误也走 `LLMResponse`，不是 Go error。** `Chat` 只在 *无法构造请求* 或 *网络层 panic* 时返回非 nil error。所有 HTTP 4xx/5xx 都是 `LLMResponse{FinishReason: "error", ...}`。这让 retry 信息（ErrShouldRetry）可以从 Provider 走到 Runner（s04）再到 Hook（s09）。
2. **`AssistantBlocks` 是把 wire 格式 *原样* 保留**。Loop append 这个回去，protocol 要求 tool_use blocks 必须以原始结构出现在历史里。如果 Loop 自己从 `Content` 和 `ToolCalls` 重建 blocks，会丢失 thinking 和其它将来才支持的 block 类型。
3. **`classifyRetry` 用前缀字符串判断 quota vs rate-limit**。这是从上游 `_NON_RETRYABLE_429_ERROR_TOKENS` 摘录的最小子集——production 的 nanobot 有 8 类 429 区分，我们这里 4 个就够教学。
4. **`FakeProvider` 是测试 backbone**。它只是一个 `[]LLMResponse` 序列。后续 s04..s11 的所有 LLM-related 测试都走 FakeProvider，不需要 API key。

## What Changed / 与上一节的变化

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

  // Loop body — switching on resp.FinishReason instead of resp.StopReason:
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

`AnthropicProvider` 增重一些（拿走了 Loop 里曾经的 wire-format 解析）；`Loop` 反而瘦下来——它不再认识"Anthropic"这个词。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s03-provider-abstraction

go run . -v "list .go files"

# 测试覆盖了 retry classification、FakeProvider、Provider error 包装
go test -v ./...

# 故意触发一个 quota 错误（401 because key wrong）看 LLMResponse 错误形态
ANTHROPIC_API_KEY=invalid go run . "test" 2>&1 | head -5
```

期望输出形态：

```
$ go run . -v "list .go files"
[s03] model=claude-sonnet-4-6 tools=1 max_turns=20
[turn 0] assistant: I'll list the .go files.
[turn 0] -> bash map[command:ls *.go]
[turn 0] <- bash_tool.go loop.go main.go provider.go provider_anthropic.go provider_fake.go registry.go tools.go
[turn 1] assistant: Eight .go files: ...
```

## Upstream Source Reading / 上游源码阅读

上游 `nanobot/providers/base.py` 里的 `LLMProvider` ABC + `LLMResponse` dataclass + `ToolCallRequest` dataclass 是这一节的核心参考。完整注解版在 [`upstream-readings/s03-provider-abstraction.py`](../../upstream-readings/s03-provider-abstraction.py)。

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

**对照阅读要点**：

- **`thinking_blocks` 我们没做**——Anthropic 的 chain-of-thought 是 `<thinking>...</thinking>` 块；上游可以转发到 Hook。我们 s09 教 Hook，但不做 thinking 转发。延伸练习。
- **`should_execute_tools` 防止 refusal/content_filter 下注入的 tool_calls 被执行**——production 安全考虑（issue #3220）。我们 s03 简化，只看 `FinishReason == "tool_calls"`。
- **上游 `LLMProvider.chat` 的内置 retry 政策**（`_CHAT_RETRY_DELAYS = (1, 2, 4)` 等）我们没在 s03 实现——s04 的 Runner 接管这个责任，让 Provider 单纯做"调一次 + 返回 LLMResponse"。
- **`to_openai_tool_call`** 是 Phase G 的关键——同一个 `ToolCallRequest` 可以序列化成 OpenAI 格式 `{"id": ..., "type": "function", "function": {"name": ..., "arguments": <json-string>}}`。
- **`error_kind` vs `error_should_retry`**——上游分了 `timeout` / `connection` / `rate_limit` / `quota` 等多种错误 kind；我们 s03 简化为只 `error_should_retry: bool`。production 想看具体 kind 的话需要扩展。

**想读更多**：
1. `nanobot/providers/base.py` 看完整 `LLMProvider` 定义，特别是 `_TRANSIENT_ERROR_MARKERS` / `_NON_RETRYABLE_429_ERROR_TOKENS` 这两个 frozenset
2. `nanobot/providers/anthropic_provider.py` 看 `_build_kwargs` 和 `_parse_response`——它们是我们 `provider_anthropic.go` 的 Python 镜像
3. `nanobot/providers/openai_compat_provider.py`——Phase G 的"另一种实现"参考

---

**下一节预告**：s04 把 Loop 的"调用 LLM ↔ 执行 tools"内核切出来变成 `Runner`，准备让 s11 把它包到外层状态机里。Loop 这个名字会被 park 到 s11。
