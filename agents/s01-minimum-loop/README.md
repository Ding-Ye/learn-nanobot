# s01 · minimum-loop

> 用 ~320 行 Go 写出 nanobot 的"最小 agent loop"。一个 provider，一个 bash 工具，一个 turn cap。
> The whole "agent" idea fits in three files.

## Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...

go run . "list the .go files in this directory"

# 加 -v 看每一步
go run . -v "echo hello | tr a-z A-Z"
```

## Files / 文件

| File | Role |
|---|---|
| `provider.go` | `Provider` interface + `AnthropicProvider` (HTTP client to `api.anthropic.com/v1/messages`) |
| `tools.go` | `Tool` interface + `BashTool` |
| `loop.go` | `Loop.Run` — the for-loop that calls the provider, executes tool_use, and stops on end_turn |
| `main.go` | CLI flag parsing + wiring |
| `loop_test.go` | unit tests with a `fakeProvider` (no real HTTP) |

## Test / 测试

```bash
go test -v ./...
```

All tests use a fake provider; no API key required for tests.

## Teaching points / 教学要点

1. The agent loop is **a `for` loop with a turn cap**. Everything else (sessions, memory, skills, hooks, state machine) is sugar around this core.
2. The `Provider` interface is the seam where Phase G will plug in OpenAI-compatible providers without touching `loop.go`.
3. Tool results travel back to the LLM as a *user* message with `tool_result` blocks — that's an Anthropic-API quirk worth memorizing.
4. We treat tool exit-non-zero as content (the model can reason about stderr) rather than as a Go error. Upstream nanobot does the same.

## What's next / 下一节

s02 introduces a `Registry` so tool dispatch leaves the loop. By s04 the Loop becomes a `Runner`; by s11 a state machine wraps everything.
