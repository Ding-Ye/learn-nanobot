## s10 · consolidator-autocompact

> 当 session 历史超出预算时，`Consolidator` 选一个**合法 user-turn 边界**、用 LLM 把那段消息压成一段话、写到 `<sessionKey>-summary.md`，并把 `Session.LastConsolidated` 推进到边界 End。`AutoCompact` 是后台 TTL ticker：扫描 `SessionManager`，给闲置时间超过 TTL 的 session 起 goroutine 跑 Consolidator，对前台 turn 完全异步。
> When a session's history blows the budget, `Consolidator` picks a **legal user-turn boundary**, asks the LLM to compress those messages into a paragraph, persists the result to `<sessionKey>-summary.md`, and advances `Session.LastConsolidated` past the boundary. `AutoCompact` is the background TTL ticker: scans `SessionManager`, spawns a Consolidator goroutine for any session idle longer than TTL, fully decoupled from the foreground turn.

### Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -workspace /tmp/agent-demo -autocompact-ttl 1m -v "list the .go files in this directory"
# [s10] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s10] loaded 2 skill(s) ...
# [s10] autocompact TTL: 1m0s
# [s10] system prompt: 612 chars; messages: 1
# [iter 0] -> bash {command:ls *.go}
# [iter 0] <- ...
# [s10] hook recorded 1 tool invocation(s): [bash]
# Files in this directory: ...
```

### Files / 文件

| File | What's new vs s09 |
|---|---|
| `provider*.go`, `tools.go`, `bash_tool.go`, `registry.go`, `runner.go`, `bus.go`, `session.go`, `skills.go`, `context.go`, `hook.go` | unchanged — carried over verbatim |
| `memory.go` | **modified** — adds `WriteSummary(sessionKey, summary)` + `ReadSummary(sessionKey)` |
| `consolidator.go` | **new** — `Boundary`, `Consolidator`, `PickBoundary`, `Consolidate`, `ErrNoLegalBoundary` |
| `autocompact.go` | **new** — `AutoCompact`, `CheckExpired`, `Run` (ticker loop) |
| `consolidator_test.go` | 5 tests (4 required + tool_result skip invariant) |
| `autocompact_test.go` | 4 tests (3 required + Run-loop ctx-cancel) |
| `main.go` | wires the consolidator, spawns `AutoCompact.Run` in a goroutine, `-autocompact-ttl` flag |

### The legal-boundary rule / 合法边界规则

```
session.Messages = [
  user, assistant, user, assistant,    # 0..3
  tool_result, assistant,              # 4..5  ← cannot cut between these
  user, assistant, user                # 6..8
]

PickBoundary(start=0, tokensToRemove=N) walks forward:
  - records every user message at idx > start where role == "user"
    AND content is NOT all tool_result blocks
  - returns the LAST candidate that hits the token threshold
  - returns false if no legal boundary exists

Why? Anthropic's protocol requires:
  - tool_use must be followed by matching tool_result
  - assistants cannot end the message stream mid-thought
The only safe cut is right before a fresh user prompt.
```

### Token estimation trade-off / Token 估算的取舍

```go
// Default placeholder (what s10 ships):
func defaultTokenEst(s string) int { return len(s) / 4 }

// Production swap (what upstream uses via tiktoken):
//   import "github.com/pkoukk/tiktoken-go"
//   c.TokenEst = func(s string) int { ... encode ... return len(tokens) }
```

`len(s)/4` is within ~2x of real BPE token counts for English; for code-heavy prompts it's off by 3-4x. We expose `TokenEst` as a function field so production callers swap in real tiktoken without forking the consolidator.

### AutoCompact / 后台压缩

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

go autocompact.Run(ctx, 30*time.Second, func() map[string]bool {
    return bus.ActiveKeys()  // sessions currently held by Bus turns
})
```

Three invariants the implementation keeps:

- **Fire-and-forget** — `CheckExpired` returns the list of session keys it queued; the goroutines run in the background and do not block the agent loop.
- **Idempotent within a tick** — two consecutive `CheckExpired` calls won't double-queue the same session because of the `archiving` map.
- **Active sessions are sacred** — sessions in the `activeSessionKeys` snapshot are skipped, so the consolidator never races the Bus on `Session.Messages`.

### Test / 测试

```bash
go test -count=1 -race ./...
```

Required tests:

- `TestConsolidator_PickBoundary_LegalUserTurnOnly` — boundary walks past assistants until it hits a real user turn.
- `TestConsolidator_Consolidate_PersistsSummary` — `FakeProvider` returns "summary text"; assert `<key>-summary.md` was written.
- `TestConsolidator_AdvancesLastConsolidated` — `s.LastConsolidated` advances to `boundary.End`.
- `TestConsolidator_NoLegalBoundary_ReturnsFalse` — short session, large `tokensToRemove`; `ok=false` and `Consolidate` returns `ErrNoLegalBoundary`.
- `TestAutoCompact_IdleSession_GetsCompacted` — idle session, empty active set, summary materializes on disk.
- `TestAutoCompact_ActiveSession_Skipped` — session in active set is skipped; provider never called.
- `TestAutoCompact_AlreadyArchiving_Skipped` — back-to-back `CheckExpired`; only one goroutine starts (counted via slow provider).

Plus extra coverage: `TestConsolidator_PickBoundary_SkipsToolResultMessages` (the boundary picker MUST NOT cut on a tool_result message even if its role is "user") and `TestAutoCompact_RunLoop_StopsOnContextCancel` (Run terminates cleanly on ctx.Done).

### Teaching points / 教学要点

1. **Legal user-turn boundary** — you can't summarize at any cut-point. Anthropic's protocol requires tool_use/tool_result pairs to flow correctly; the safe cut is "right before a fresh user prompt".
2. **Token estimation is approximate** — `len(s)/4` is a placeholder; production swaps via `c.TokenEst = realTiktoken`.
3. **AutoCompact is fire-and-forget** — spawns goroutines, doesn't block the agent loop. Idempotent because of the `archiving` map.
4. **Summaries persist as separate files**, not in-place edits to history. App. A's "memory as interpretation" point: append-only history (audit) vs curated summary (interpretation) live in different files.

### Next / 下一节

s11 — composing everything into the TurnState state machine (RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE).
