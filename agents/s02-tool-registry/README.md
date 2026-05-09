# s02 · tool-registry

> 把 `Tool` 的查找/调度/缓存从 `Loop` 里搬出来。`Loop` 此后只决定"调谁"，`Registry` 管"怎么找到 + 怎么排序 + 怎么 dispatch"。
> Carve tool lookup, dispatch, and schema caching out of `Loop`. The loop only decides *who* to call; the Registry handles *how to find them*.

## Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "list .go files"
```

## Files / 文件

| File | What's new vs s01 |
|---|---|
| `provider.go` | unchanged from s01 |
| `tools.go` | now contains *only* the `Tool` interface |
| `bash_tool.go` | `BashTool` extracted to its own file |
| `registry.go` | **new** — `Registry`, `Register`, `Unregister`, `Get`, `Execute`, `Definitions()` |
| `loop.go` | `Loop.Tools` field changed from `[]Tool` to `*Registry`; dispatch goes through `Registry` |
| `main.go` | wires `Registry` instead of building a `[]Tool` slice |
| `registry_test.go` | **new** — caching, ordering, unknown-tool, error-as-content tests |
| `loop_test.go` | end-to-end with the new Registry; verifies unknown-tool doesn't abort |

## Test / 测试

```bash
go test -v ./...
```

## Teaching points / 教学要点

1. **Caching matters for prompt-cache hit rate.** Same tools in same order = same prompt prefix = Anthropic prompt cache hit on every turn after the first. Re-sorting the schema list unnecessarily would invalidate that.
2. **Built-in tools first, MCP tools second** — both name-sorted within their group. Same convention as upstream nanobot's `ToolRegistry.get_definitions()`.
3. **Tool errors become content; only "unknown tool" becomes a typed error** — and the loop catches that to write a `tool_result` instead of aborting.
4. **`Execute` invalidates nothing.** The cache is keyed on register/unregister only — schemas don't change at execute time.

## What's next / 下一节

s03 reshapes `Provider` itself: instead of returning Anthropic's wire-format `CreateMessageResponse`, it returns a normalized `LLMResponse` with `Content`/`ToolCalls`/`FinishReason`. That's the seam Phase G will use to plug in OpenAI-compat providers.
