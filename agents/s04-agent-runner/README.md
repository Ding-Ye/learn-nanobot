# s04 · agent-runner

> 把 s03 的 `Loop` 内核切出来变成 `Runner`：吃 `AgentRunSpec`（caller 准备好的 messages + Tools + Model + MaxIterations），吐 `AgentRunResult`（FinalContent + Messages + ToolsUsed + Usage + FinishReason）。同时引入"length 恢复"——上游 `_handle_length_recovery` 的最小版本。
> Carve s03's `Loop` core into a narrower `Runner` that consumes a structured `AgentRunSpec` and returns a structured `AgentRunResult`. Add length recovery — a single-shot version of upstream's `_handle_length_recovery`.

## Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -v "list .go files"
```

## Files / 文件

| File | What's new vs s03 |
|---|---|
| `provider.go`, `provider_anthropic.go`, `provider_fake.go`, `tools.go`, `bash_tool.go`, `registry.go` | unchanged — carried over verbatim because each session is its own Go module |
| `runner.go` | **new** — `AgentRunSpec` / `AgentRunResult` / `Runner.Run`. Replaces s03's `loop.go` |
| `main.go` | builds `AgentRunSpec` (initial messages constructed by caller, not Runner), calls `runner.Run`, prints `result.FinalContent`. Verbose mode also prints `tools_used` + token usage |
| `runner_test.go` | 6 tests covering stop / two-tool-calls / max-iterations / partial-failure / length-recovery / provider-error |
| `testdata/expected.txt` | sample run shape annotated with `tools_used` + `usage` field expectations |

## Test / 测试

```bash
go test -v ./...
```

## Teaching points / 教学要点

1. **The Runner doesn't build the prompt anymore.** s03's Loop did `messages := [{user, prompt}]` itself; in s04 the caller hands in `InitialMessages`. This is the seam where s08's `ContextBuilder` will plug in: it'll pre-build `[system, memory, skills, history, user]` and pass that as `InitialMessages` instead.
2. **`AgentRunResult` vs s03's bare string.** Returning `FinalContent + Messages + ToolsUsed + Usage` lets s05's session bus persist the full message log, lets s09's hooks see token totals, and lets s11 keep telemetry on which tools fired during a turn.
3. **Length recovery is single-shot.** When the provider returns `FinishReason="length"`, we append a "continuation" hint message and let the next iteration re-prompt. Upstream allows up to `_MAX_LENGTH_RECOVERIES=3`; we ship 1 to keep the idea visible.
4. **Provider errors still bail.** s09 (Hook) is where retries land. The Runner only normalizes — it never re-calls Chat after `FinishReason="error"`.

## What's next / 下一节

s05 wraps the Runner with a per-session goroutine + MessageBus, so multiple sessions can run in parallel while turns within one session stay serialized. The `Runner` shape doesn't change — Bus just calls `runner.Run(ctx, spec)` from inside each session's goroutine.
