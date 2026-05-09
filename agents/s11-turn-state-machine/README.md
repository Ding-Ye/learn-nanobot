## s11 · turn-state-machine

> 这是 numbered 章节里最大、最特别的一章——把 s01 到 s10 的所有组件，串到一个外层状态机里：`RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE`。`Loop` 这个名字回来了，但**类型完全不同**：s01 的 `Loop` 直接包 LLM，s11 的 `Loop` 包了 8 个状态，其中 RUN 状态里再调 `Runner`（也就是 s04 的内层 LLM ↔ 工具循环）。
>
> The biggest and most architecturally distinctive numbered chapter. It composes every component from s01-s10 into a single outer state machine: `RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE`. The name `Loop` is back, but the **shape is entirely different**: s01's `Loop` wrapped the LLM directly; s11's `Loop` wraps eight states, one of which (RUN) wraps the LLM via `Runner` (s04's inner tool-loop).

### Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -workspace /tmp/agent-demo -v "list the .go files here"
# [s11] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s11] loaded 2 skill(s) ...
# [s11] state: START -> RESTORE
# [s11] state: RESTORE -> COMPACT
# [s11] state: COMPACT -> COMMAND
# [s11] state: COMMAND -> BUILD
# [s11] state: BUILD -> RUN
# [iter 0] -> bash {command:ls *.go}
# [iter 0] <- ...
# [s11] state: RUN -> SAVE
# [s11] state: SAVE -> RESPOND
# [s11] state: RESPOND -> DONE
# Files: ...

# Try a slash command — short-circuits straight to RESPOND, no LLM call:
go run . -v /help
# [s11] state: RESTORE -> COMPACT
# [s11] state: COMPACT -> COMMAND
# [s11] state: COMMAND -> RESPOND   <-- skipped BUILD/RUN/SAVE
# [s11] state: RESPOND -> DONE
# Available commands:
#   /help    Show this help text.
#   /clear   Reset the current session's message history.
```

### Files / 文件

| File | What's new vs s10 |
|---|---|
| `provider*.go`, `tools.go`, `bash_tool.go`, `registry.go`, `runner.go`, `bus.go`, `session.go`, `skills.go`, `context.go`, `hook.go`, `memory.go`, `consolidator.go`, `autocompact.go` | unchanged — carried over verbatim |
| `turnstate.go` | **new** — `TurnState` enum, `TurnContext` mutable bag, `Transitions` map, `String()`, `IsTerminal()`, `advance()`, `fail()` |
| `loop.go` | **new** — `Loop` struct, `LoopConfig`, `NewLoop`, `Process` (the driver), `dispatch` |
| `state_restore.go` … `state_respond.go` | **new** — one method per state; each one composes 1-3 prior chapters' types |
| `loop_test.go` | 8 tests including the 6 required |
| `state_test.go` | 4 tests on the static graph (Transitions table integrity, String, advance/fail) |
| `main.go` | rewired — Bus's session goroutine now calls `Loop.Process(ctx, msg)` instead of `Runner.Run` directly |

### The state graph / 状态图

```
                 user message in
                       |
                       v
                  +----------+
                  | RESTORE  |    s05 (SessionManager) + s06 (MemoryStore)
                  +----+-----+    get-or-create session, load history
                       |
                       v
                  +----------+
                  | COMPACT  |    s10 (Consolidator)
                  +----+-----+    summarize older half if over budget
                       |
                       v
                  +----------+
                  | COMMAND  |    new in s11
                  +----+-----+    /help, /clear -> short-circuit to RESPOND
                       |
                       v
                  +----------+
                  |  BUILD   |    s06+s07+s08 (ContextBuilder)
                  +----+-----+    assemble system prompt + messages
                       |
                       v
                  +----------+
                  |   RUN    |    s04 (Runner) + s09 (Hook)
                  +----+-----+    the **inner** LLM ↔ tools loop
                       |
                       v
                  +----------+
                  |   SAVE   |    s06 (MemoryStore.AppendHistory)
                  +----+-----+    persist user + assistant to history.jsonl
                       |
                       v
                  +----------+
                  | RESPOND  |    new in s11
                  +----+-----+    set tc.FinalContent for the channel
                       |
                       v
                  +----------+
                  |   DONE   |    terminal
                  +----------+

         any state's error -> StateError + tc.Err set
```

The `_TRANSITIONS` table in `turnstate.go` is **data**, not a switch in code:

```go
var Transitions = map[TurnState]TurnState{
    StateRestore: StateCompact,
    StateCompact: StateCommand,
    StateCommand: StateBuild,
    StateBuild:   StateRun,
    StateRun:     StateSave,
    StateSave:    StateRespond,
    StateRespond: StateDone,
}
```

`state_test.go::TestTransitions_AllStatesReachableFromRestore` walks this map to assert every state is reachable. If you grow the state machine — add `INJECT` after `BUILD`, say — the table is the single source of truth and the test tells you immediately if you broke reachability.

### Two loops, two responsibilities / 两层循环

```
outer loop (Loop.Process):  for !tc.IsTerminal() { switch tc.State { ... } }
                             |
                             | per-turn: 8 states, one walk per user message
                             |
inner loop (Runner.Run):     for iter < MaxIter { provider.Chat(); execute tools; }
                             |
                             | per-LLM-iteration: walks tool calls until "stop"
```

- **Outer** owns: session lock, slash-command short-circuiting, history slicing, persistence, error transitions.
- **Inner** owns: provider call, tool execution, length recovery, hook calls.

Splitting them is what lets s04's `Runner` exist standalone (taught in s04 with no surrounding state machine) and lets s11 graft the conductor on top without touching s04's internals.

### Slash-command short-circuit / 斜杠命令短路

`COMMAND` is the one state allowed to **rewrite `tc.State` directly** rather than letting the driver consult the Transitions table:

```go
// state_command.go
case "help":
    tc.FinalContent = helpText
    tc.State = StateRespond  // skip BUILD/RUN/SAVE
    return nil
```

The driver loop checks `if tc.State == prev { tc.advance() }`. If COMMAND already moved the state, advance is skipped and the new target (RESPOND) is honored. That's how `/help` runs in 4 transitions instead of 7 — and crucially, **never calls the LLM**.

### Test / 测试

```bash
go test -count=1 -race ./...
```

Required tests (loop_test.go):

- `TestLoop_HappyPath_RestoreToDone` — full state walk; assert the 7 happy-path states fired in order.
- `TestLoop_CompactState_TriggersConsolidator` — pre-populate session over budget; assert Consolidator fired (2 Chat calls — consolidator + run) and a summary file was written.
- `TestLoop_CommandState_SlashHelp_ShortCircuits` — input `/help`; assert 0 Chat calls; FinalContent contains help text.
- `TestLoop_RunState_ToolUseRoundTrip` — script `tool_calls` → `stop`; assert ToolsUsed == [bash].
- `TestLoop_SaveState_PersistsTurn` — assert `history.jsonl` has ≥2 lines and `Session.Messages` has user+assistant.
- `TestLoop_ErrorInBuild_TransitionsToError` — yank the memory dir between construction and Process; assert `tc.State == StateError`, `tc.Err` non-nil, error message tagged with `BUILD`.

Plus: `TestLoop_ProcessIsIdempotentForSameSessionKey` (two turns same session), `TestLoop_NewLoopDefaults` (defaults wire correctly).

State-graph integrity tests (state_test.go):

- `TestTransitions_AllStatesReachableFromRestore` — graph traversal asserts every state is reached from RESTORE.
- `TestTransitions_NoTerminalStatesInTable` — DONE/ERROR must not have outgoing edges.
- `TestTurnState_String` — every state's String() round-trips to the right name.
- `TestTurnContext_AdvanceAndFail` — `advance()` respects the table, `fail()` sets State=Error and Err.

### Teaching points / 教学要点

1. **The name `Loop` is back, but it's a *different type*.** s01's `Loop` and s11's `Loop` are isolated by Go's package-scoping rules — each session is its own module. Same name; the shapes are entirely different. The diff cue: "It used to wrap the LLM directly. Now it wraps eight states, one of which (RUN) wraps the LLM via Runner."
2. **The state machine composes 7 prior sessions.** RESTORE=s05+s06, COMPACT=s10, COMMAND=new, BUILD=s06+s07+s08, RUN=s04+s09, SAVE=s06, RESPOND=new. Reading this chapter is the closest thing learn-nanobot has to reading nanobot itself.
3. **`_TRANSITIONS` is a map, not a switch in code.** Data is inspectable; the graph-integrity test walks the map. Add a state, the test tells you if you broke reachability.
4. **`TurnContext` is the mutable bag.** Methods read+write it; never return new copies. Mirrors upstream's Python pattern.
5. **Error transition.** Any state's handler returning a non-nil error sets `tc.State = StateError`, `tc.Err = err`. The driver bails out; `Process()` returns the wrapped error so callers see the originating state.

### Next / 下一节

s_full — the integration chapter. No new code. Read the 16-step end-to-end trace and see how each step references s01-s11.
