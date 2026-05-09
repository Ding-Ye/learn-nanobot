# s03 · provider-abstraction

> 把 Anthropic 的 wire 格式从 `Loop` 里隔离出去。`Provider` 现在返回规范化的 `LLMResponse`（Content / ToolCalls / FinishReason / Err…），任何后端只要写一个 `Provider` 实现就能接入。Phase G 多模型支持就是用这个口子加 OpenAI-compat。
> Hide Anthropic's wire format behind a normalized `LLMResponse`. New backends slot in by writing one `Provider` impl — the Loop never changes.

## Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "list .go files"
```

## Files / 文件

| File | What's new vs s02 |
|---|---|
| `provider.go` | `Provider` interface now returns `LLMResponse`; new types `ChatReq`, `LLMResponse`, `ToolCall` |
| `provider_anthropic.go` | **new** — Anthropic wire-format translator |
| `provider_fake.go` | **new** — deterministic test double, scripted responses |
| `tools.go`, `bash_tool.go`, `registry.go` | unchanged from s02 |
| `loop.go` | reads `LLMResponse.ToolCalls` / `Content` / `FinishReason` instead of `resp.Content[].Type` |
| `provider_test.go` | retry classification, FakeProvider, error-response propagation |

## Test / 测试

```bash
go test -v ./...
```

## Teaching points / 教学要点

1. `LLMResponse.AssistantBlocks` carries the original Anthropic-shaped blocks back to the Loop so it can append them to history. Other providers (OpenAI in Phase G) synthesize equivalent blocks. **The protocol requires them; don't try to drop them.**
2. `FinishReason` is normalized across providers: `stop` / `tool_calls` / `length` / `error`. Same vocabulary regardless of backend.
3. `ErrShouldRetry` makes 429-quota vs 429-rate-limit distinguishable. Production retries the latter, surfaces the former.
4. `FakeProvider` is the test double every later session reuses. No tests in s03..s11 talk to a real LLM.

## What's next / 下一节

s04 splits the Loop into a thin `Runner` (LLM ↔ tools) — same logic, more modular. The name `Loop` is parked until s11 reuses it for a different purpose (the outer state machine).
