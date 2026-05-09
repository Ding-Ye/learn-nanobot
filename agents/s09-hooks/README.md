## s09 · hooks

> 给 Runner 加 5 个生命周期 tap 点：`BeforeIter` / `OnStream` / `BeforeExecuteTools` / `AfterIter` / `FinalizeContent`。`CompositeHook` 提供 fan-out + 错误隔离；`SDKCaptureHook` 是默认的可观测 hook（累积 `tools_used` 和 final messages）。
> Five lifecycle tap points on the Runner: `BeforeIter` / `OnStream` / `BeforeExecuteTools` / `AfterIter` / `FinalizeContent`. `CompositeHook` fans out with error isolation; `SDKCaptureHook` is the default observability hook (records `tools_used` + final messages).

### Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -workspace /tmp/agent-demo -skills weather -v "Tokyo weather"
# [s09] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s09] loaded 2 skill(s) ...
# [s09] system prompt: 612 chars; messages: 1
# [iter 0] -> bash {command:...}
# [iter 0] <- ...
# [s09] hook recorded 1 tool invocation(s): [bash]
# ...
```

### Files / 文件

| File | What's new vs s08 |
|---|---|
| `provider*.go`, `tools.go`, `bash_tool.go`, `registry.go`, `context.go`, `bus.go`, `session.go`, `memory.go`, `skills.go` | unchanged — carried over verbatim |
| `hook.go` | **new** — `Hook` interface, `HookCtx`, `NoopHook`, `CompositeHook` (with error isolation), `SDKCaptureHook` |
| `runner.go` | **modified** — adds `Hook` field; calls hooks at 5 well-defined moments |
| `hook_test.go` | 6 tests (5 required + 1 default-noop sanity check) |
| `main.go` | wires `SDKCaptureHook`; prints `len(captureHook.ToolsUsed)` summary |

### The 5 hook tap points / 5 个挂载点

```
Runner.Run iteration:
  BeforeIter         ← hook can stash setup state, log, or return error to abort
  (LLM call)
  if response wants tools:
    BeforeExecuteTools ← hook can return error to short-circuit (no tools run)
    (run tools)
  AfterIter          ← always fires, even on tool_calls / length recovery
                       SDKCaptureHook records tool_calls + messages here

After loop ends (stop / length / max_iterations):
  FinalizeContent    ← hook return value replaces what caller sees
                       upstream's contract: NOT error-isolated
```

### CompositeHook & error isolation / 复合 hook 与错误隔离

```go
runner.SetHook(NewCompositeHook(
    captureHook,    // default observability
    telemetryHook,  // your custom one
    redactHook,     // FinalizeContent rewrite
))
```

Per upstream's contract:

- BeforeIter / OnStream / BeforeExecuteTools / AfterIter — wrapped in `recover()`. A panicking hook is logged, the rest of the registered hooks still run.
- FinalizeContent — *not* error-isolated. Bugs in finalization should fail loudly rather than corrupt the response.

### SDKCaptureHook / 默认观测 hook

```go
captureHook := NewSDKCaptureHook()
runner.SetHook(captureHook)
runner.Run(ctx, spec)
fmt.Println(captureHook.ToolsUsedSnapshot())   // [bash bash web_search ...]
fmt.Println(captureHook.FinalMessagesSnapshot())
```

Mirrors upstream's `SDKCaptureHook`: records every tool name across iterations (duplicates preserved), and snapshots the message list at the end of each iteration so the last snapshot is the end-of-turn state.

### Test / 测试

```bash
go test -count=1 -race ./...
```

The 5 required tests:

- `TestHook_BeforeIter_AfterIter_CalledInOrder` — 2-turn loop; assert call order is `[BeforeIter(0), BeforeExecuteTools(0), AfterIter(0), BeforeIter(1), AfterIter(1), FinalizeContent]`.
- `TestCompositeHook_OneFails_OthersStillRun` — hook A panics in BeforeIter, hook B records; assert B's BeforeIter still ran.
- `TestSDKCaptureHook_AccumulatesToolsUsed` — script with 2 tool_calls; assert SDKCaptureHook.ToolsUsed has both entries.
- `TestHook_FinalizeContent_CanRewrite` — hook prepends "rewritten:"; assert final result has prefix.
- `TestHook_BeforeExecuteTools_CanShortcircuit` — sentinel error from BeforeExecuteTools; runner bails cleanly, tools never run.

### Teaching points / 教学要点

1. **Five tap points, not one.** Hooks aren't just "log when LLM responds" — they're at every interesting moment. Mid-flight cancel lives at BeforeExecuteTools; end-of-turn rewrite lives at FinalizeContent; per-iteration observability lives at AfterIter.
2. **Error isolation is per-method.** Fan-out methods recover panics so one bad hook can't break the others. FinalizeContent is the exception — finalization bugs surface.
3. **`SDKCaptureHook` is the default observability layer.** SDK callers don't have to write their own hook to see tools_used; just register it and read fields after Run returns.
4. **Hooks compose, Runners don't.** Runner takes a single Hook; CompositeHook is the "many hooks at once" pattern. Keeps the Runner simple and the composition explicit.
5. **Hooks are the seam for streaming, telemetry, mid-flight cancel, and sideband injection.** All four are extension exercises (App. B); s09 lays the cable.

### Next / 下一节

s10 — Consolidator + AutoCompact for memory compression. When the session history gets too long, the LLM summarizes the oldest messages into a one-paragraph block and advances `Session.LastConsolidated`. AutoCompact is the TTL ticker that runs the consolidator on idle sessions in the background.
