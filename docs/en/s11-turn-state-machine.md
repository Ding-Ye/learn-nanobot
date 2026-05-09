---
title: "s11 · Turn State Machine"
chapter: 11
slug: s11-turn-state-machine
est_read_min: 36
---

# s11 · Turn State Machine

> What this teaches: from s01 through s10 we built ten components — provider, tool, runner, session, bus, memory, skills, context, hooks, consolidator — but `main.go` has been wiring them ad-hoc. The upstream answer is a real outer state machine: `RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE`, transitions in a dict (not a `switch` in code), per-session lock owned by it. The name `Loop` is back, but the **shape is entirely different**: s01's `Loop` wrapped the LLM directly; s11's `Loop` wraps eight states, one of which (RUN) wraps the LLM via `Runner` (s04's inner tool-loop). **Two loops, two responsibilities.** Once you see this, the entire nanobot architecture clicks.

---

## Problem / The Setup

After s01–s10 we have ten components: a `Provider` abstraction (s03), a `Tool` interface plus `Registry` (s02), the `Runner` inner loop (s04), `Session` and `Bus` (s05), `MemoryStore` for file persistence (s06), `SkillLoader` for markdown skills (s07), `ContextBuilder` for system-prompt assembly (s08), `Hook` for lifecycle observability (s09), `Consolidator` plus `AutoCompact` for memory compression (s10). Each chapter built one component, added some tests, and main.go wired them by hand — read user input, look up the session, build the prompt, run the runner, persist history, print the reply.

That hand-wiring pattern starts to bite after s10. Each new component adds a new clause to main.go: s06 wires the MemoryStore, s07 adds a SkillLoader, s08 plumbs a ContextBuilder, s09 attaches a hook, s10 spawns the AutoCompact goroutine. `main.go` is now 264 lines of "if this is nil skip it, otherwise call that" plumbing. **There's no single place to look at one turn's lifecycle.** Adding any new behavior touches several spots in main.go. Error handling is scattered. Slash commands? You can't short-circuit the LLM call until the input has arrived, but where does the short-circuit live? Where does the session lock get acquired? Where do you trigger foreground compact?

Upstream answers all of this by modeling the entire turn lifecycle as a state machine:

```
RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE
```

Seven non-terminal states plus DONE. Each state is a single method. Transitions between states are declared in a `_TRANSITIONS: dict[(TurnState, str), TurnState]` table — that's **data**, not code. The state-machine driver is a simple `for` loop: take the current state, look up dispatch, call the method, decide the next state.

Why does this win? Three reasons:

1. **Readability.** Want to know what happens during a turn? Read seven `_state_X` methods. Each method does one specific thing; all the signatures are `(ctx: TurnContext) -> str`. A new reader scans `loop.py` top-to-bottom and constructs the full agent lifecycle in their head.

2. **Testability.** The state graph is data — you can test it independently. `TestTransitions_AllStatesReachableFromRestore` walks the table as a graph and asserts every state is reachable from RESTORE. If you add an `INJECT` state later and forget to wire it in, that test fires immediately. With a `switch` block the same knowledge is scattered across the call chain — there's no automated way to check it.

3. **Separation of concerns.** Each state owns its dependencies. COMPACT owns the Consolidator; BUILD owns the ContextBuilder; RUN owns the Runner. New behavior lives at state boundaries ("insert an INJECT state after BUILD to receive sideband injections"), not buried in a mega-method.

There is one thing readers from s01 will hesitate at — **the name `Loop` is back, but the type is entirely different**:

- s01.Loop wraps `Provider Provider; Tools *Registry; ...`. Its method is `(l *Loop) Step(...)`. Its job is "call the LLM once".
- s11.Loop wraps `Runner *Runner; CtxBuilder *ContextBuilder; Sessions *SessionManager; Memory *MemoryStore; Bus *Bus; Consolidator *Consolidator; Hook Hook`. Its method is `(l *Loop) Process(ctx, msg) (*TurnContext, error)`. Its job is "drive eight states through one turn".

They share a name because each session is its own Go module — s01.Loop and s11.Loop are **literally different types** by Go's package-scoping rules; the compiler never confuses them. We deliberately reuse the name because (a) upstream uses it (`nanobot/agent/loop.py::AgentLoop`), and (b) `Loop` is the natural name for this conductor — withholding it just to avoid a name collision would be an unforced error.

The diff cue to remember: "the name `Loop` is back. It used to wrap the LLM directly. Now it wraps eight states, one of which (RUN) wraps the LLM via `Runner`. Reading this chapter top-to-bottom is the closest thing learn-nanobot has to reading nanobot itself."

## Solution / The Implementation

`turnstate.go` introduces one enum + one mutable bag + one transitions table:

```go
type TurnState int

const (
    StateRestore TurnState = iota
    StateCompact
    StateCommand
    StateBuild
    StateRun
    StateSave
    StateRespond
    StateDone
    StateError
)

func (s TurnState) String() string { /* "RESTORE", "COMPACT", ... */ }

var Transitions = map[TurnState]TurnState{
    StateRestore: StateCompact,
    StateCompact: StateCommand,
    StateCommand: StateBuild,
    StateBuild:   StateRun,
    StateRun:     StateSave,
    StateSave:    StateRespond,
    StateRespond: StateDone,
}

type TurnContext struct {
    Msg              InboundMessage
    SessionKey       string
    State            TurnState
    Session          *Session
    History          []Message
    SystemPrompt     string
    InitialMessages  []Message
    Result           *AgentRunResult
    FinalContent     string
    ToolsUsed        []string
    Generated        []string
    Err              error
}

func (tc *TurnContext) IsTerminal() bool { return tc.State == StateDone || tc.State == StateError }
func (tc *TurnContext) advance() bool { ... }
func (tc *TurnContext) fail(err error) { tc.Err = err; tc.State = StateError }
```

`loop.go` introduces the outer state machine:

```go
type Loop struct {
    Runner       *Runner
    Tools        *Registry
    CtxBuilder   *ContextBuilder
    Sessions     *SessionManager
    Memory       *MemoryStore
    Bus          *Bus
    Consolidator *Consolidator
    Hook         Hook
    Model        string
    MaxIter      int
    CompactBudget int
    Verbose      bool
}

func NewLoop(cfg LoopConfig) *Loop
func (l *Loop) Process(ctx context.Context, msg InboundMessage) (*TurnContext, error)
func (l *Loop) dispatch(ctx context.Context, tc *TurnContext) error
```

The core of `Process` is the driver loop:

```go
for !tc.IsTerminal() {
    prev := tc.State
    err := l.dispatch(ctx, tc)
    if err != nil {
        tc.fail(fmt.Errorf("%s: %w", prev, err))
        break
    }
    if tc.State == prev {
        // handler didn't override state — advance via the table
        if !tc.advance() { tc.fail(...); break }
    }
    // else: handler set state itself (e.g. COMMAND short-circuiting to RESPOND); respect it
}
```

Seven state files, one per state:

- `state_restore.go` — `tc.Session = l.Sessions.GetOrCreate(tc.SessionKey)`. Composes s05.
- `state_compact.go` — estimate the un-consolidated tail; if over budget call `l.Consolidator.Consolidate`. Composes s10.
- `state_command.go` — `/help` writes helpText to `tc.FinalContent` and sets `tc.State = StateRespond`; `/clear` resets Session.Messages, then jumps to RESPOND; everything else passes through. New state.
- `state_build.go` — calls `l.CtxBuilder.BuildSystemPrompt` and `BuildMessages`; writes the result to `tc.SystemPrompt` and `tc.InitialMessages`. Composes s06+s07+s08.
- `state_run.go` — `l.Runner.Run(ctx, spec)`; writes the result to `tc.Result`. Composes s04+s09.
- `state_save.go` — `l.Memory.AppendHistory` writes two entries (user + assistant) and `tc.Session.AppendAll(post-turn-messages)`. Composes s06.
- `state_respond.go` — if `tc.FinalContent` was set by COMMAND, keep it; otherwise copy from `tc.Result.FinalContent`. New state.

`main.go`'s Bus call switches from:

```go
result, err := runner.Run(ctx, spec)  // s10 shape
```

to:

```go
tc, err := loop.Process(ctx, msg)     // s11 shape
finalContent := tc.FinalContent
```

## How It Works / Walking Through the Code

Read `Process` once:

```go
func (l *Loop) Process(ctx context.Context, msg InboundMessage) (*TurnContext, error) {
    tc := &TurnContext{
        Msg:        msg,
        SessionKey: msg.SessionKey,
        State:      StateRestore,
    }
    for !tc.IsTerminal() {
        prev := tc.State
        err := l.dispatch(ctx, tc)
        if err != nil {
            tc.fail(fmt.Errorf("%s: %w", prev, err))
            break
        }
        if tc.State == prev {
            if !tc.advance() {
                tc.fail(fmt.Errorf("no transition from %s", prev))
                break
            }
        }
    }
    if tc.State == StateError {
        return tc, tc.Err
    }
    return tc, nil
}
```

Four things happen:

1. **Seed the TurnContext.** Msg, SessionKey, starting State=Restore. Every other field is zero — the states fill them in.
2. **Run the driver loop.** Each iteration records the current state, dispatches, then decides what's next.
3. **Handle transitions.** If dispatch errored → mark ERROR + exit. Else if the handler overrode state (short-circuit case) → respect it; otherwise consult the Transitions table to advance.
4. **Return tc.** Callers get the full TurnContext including FinalContent, Result, ToolsUsed, and the terminal State.

Each state composes prior-session components:

```
RESTORE  composes s05 + s06
COMPACT  composes s10
COMMAND  new — slash-command short-circuit
BUILD    composes s06 + s07 + s08
RUN      composes s04 + s09
SAVE     composes s06
RESPOND  new — final string assembly
```

The whole point of the outer Loop is **orchestrating components built in earlier chapters** — every state is at most ~50 lines of code because the actual heavy lifting is delegated to those components. The bulk of the LOC in this chapter is the comments in turnstate.go (explaining why mutable bag, why map vs switch) and the driver in loop.go (explaining why handlers can write tc.State directly).

**Two loops, two responsibilities.** This is the mental model to internalize.

```
outer loop (Loop.Process):  for !tc.IsTerminal() { switch tc.State { ... } }
                             one walk per turn: 8 states, each a method call

inner loop (Runner.Run):    for iter < MaxIter { provider.Chat(); execute tools; }
                             one walk per LLM iteration: provider call → tool call →
                             provider call → ... until finish_reason="stop"
```

Outer owns: session lock, slash-command short-circuit, history slicing, persistence, error transitions.
Inner owns: provider call, tool execution, length recovery, hook calls.

Why split them? Because outer concerns — locks, file I/O, command dispatch — are slow, IO-bound, explicit. They don't belong inside the inner LLM loop where every additional branch makes the call site harder to reason about. Splitting keeps each loop honest about its own responsibilities.

Why a map for `_TRANSITIONS` and not a switch? **Data is inspectable.** `state_test.go::TestTransitions_AllStatesReachableFromRestore` walks the table as a graph and asserts every state is reachable. If you grow the state machine — add `INJECT` after `BUILD`, say — the table is the single source of truth and the test tells you immediately if you broke reachability. A `switch` block buries that knowledge in code paths.

The tests verify **the chapter's core composition claim**: the same components, wired through the state machine, behave the way the state graph predicts:

```go
// loop_test.go::TestLoop_HappyPath_RestoreToDone
//
// Run a normal user message through FakeProvider, record the state visited
// each tick, assert the order is [RESTORE, COMPACT, COMMAND, BUILD, RUN,
// SAVE, RESPOND].
```

```go
// loop_test.go::TestLoop_CommandState_SlashHelp_ShortCircuits
//
// Input "/help". FakeProvider's script is nil — any call would fail with
// "script exhausted". Assert Process completes, tc.State == StateDone,
// tc.FinalContent contains helpText, fp.Calls() == 0 (**LLM never called**).
```

```go
// loop_test.go::TestLoop_CompactState_TriggersConsolidator
//
// Pre-populate a session so the un-consolidated tail estimate exceeds
// CompactBudget. FakeProvider supplies two responses (one for the
// consolidator's summary, one for the actual turn). Assert fp.Calls() ==
// 2 and assert mem.ReadSummary returns the expected string.
```

`TurnContext` is a mutable bag — every state reads + writes it; it's never copied. This mirrors upstream's Python dataclass pattern. The Go alternative would be `(*TurnContext, error)` returns from each handler, but then the driver has to re-thread the pointer every iteration, and fields end up duplicated across handlers. The mutable bag lets handlers look like normal methods — read `tc.Session`, write `tc.SystemPrompt` — while the driver holds a single pointer end to end.

The tradeoff: a handler that errors mid-write can leave the bag inconsistent. We mitigate via **transactional handlers**: each handler either fully populates its outputs OR sets State=Error + Err. The driver checks State on every iteration, so a partially-mutated bag still terminates cleanly.

**Error transition.** Any state's handler returning a non-nil error has the driver:

```go
tc.fail(fmt.Errorf("%s: %w", prev, err))
```

`tc.Err` is wrapped with the originating state ("BUILD: stat ...", "RUN: provider ..."), `tc.State` is set to StateError, the outer loop's IsTerminal becomes true and exits. `Process` returns `(tc, tc.Err)` — the caller gets both the error AND the tc's last-known state, which is critical for debugging. `TestLoop_ErrorInBuild_TransitionsToError` exercises this path by yanking the memory directory off disk between construction and Process; it asserts `tc.State == StateError`, `tc.Err != nil`, and that the error message contains the "BUILD" tag.

**Slash-command short-circuit.** `COMMAND` is the one state allowed to rewrite `tc.State` directly:

```go
case "help":
    tc.FinalContent = helpText
    tc.State = StateRespond  // skip BUILD/RUN/SAVE
    return nil
```

The driver detects `tc.State != prev` and respects the handler's choice (without consulting Transitions). That's how `/help` runs in 4 transitions instead of 7 — and crucially, **it never calls the LLM**.

## What Changed / vs the Previous Chapter

No new low-level component lands in s11 — every prior-session .go file is carried over verbatim. The new code is all in the composition layer:

- `turnstate.go` (**new**) — `TurnState` enum + `TurnContext` struct + `Transitions` map + small helpers (`String`, `IsTerminal`, `advance`, `fail`).
- `loop.go` (**new**) — `Loop`, `LoopConfig`, `NewLoop`, `Process`, `dispatch`. This `Loop` is **a different type from s01's Loop**: s01.Loop wraps the LLM directly; s11.Loop wraps eight states, one of which (RUN) wraps the LLM via Runner. Go's package scoping makes the two types literally distinct.
- `state_restore.go`, `state_compact.go`, `state_command.go`, `state_build.go`, `state_run.go`, `state_save.go`, `state_respond.go` (**new**) — seven state methods, one file per state. Each is short (30–100 lines) because the heavy work delegates to prior-session components.
- `loop_test.go` (**new**) — 8 tests including the 6 required (HappyPath, Compact, Command, Run, Save, ErrorInBuild) plus two extras (idempotent multi-turn, defaults wiring).
- `state_test.go` (**new**) — 4 static-graph integrity tests: reachability, no-terminal-states-in-table, full String coverage, advance/fail helpers.
- `main.go` (**rewritten**) — wires up the `Loop`; calls `loop.Process(ctx, msg)`. Verbose mode prints every state transition: `[s11] state: RESTORE -> COMPACT`. You can watch the state graph being walked in the output.
- All other .go files (**unchanged**) — provider*, tools.go, bash_tool.go, registry.go, runner.go, bus.go, session.go, skills.go, context.go, hook.go, memory.go, consolidator.go, autocompact.go are carried over as-is.

LOC estimate (with comments):

| File | LOC | Role |
|---|---|---|
| `turnstate.go` | ~270 | state-machine vocabulary (enum + bag + table + helpers) |
| `loop.go` | ~265 | driver (`Process` + `dispatch`) |
| `state_*.go` × 7 | ~530 total | state implementations |
| `loop_test.go` | ~400 | 8 tests |
| `state_test.go` | ~150 | 4 tests |
| `main.go` | ~230 | CLI wiring |
| **new code total** | **~1840** | (prod ~1300 + tests ~550) |

## Try It / Hands-On

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s11-turn-state-machine

# Normal turn — walks all seven states
go run . -workspace /tmp/s11-demo -v "list the .go files here"
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

# Slash command — short-circuits to RESPOND, never calls the LLM
go run . -v /help
# [s11] state: RESTORE -> COMPACT
# [s11] state: COMPACT -> COMMAND
# [s11] state: COMMAND -> RESPOND   <-- skipped BUILD/RUN/SAVE
# [s11] state: RESPOND -> DONE

# Run the tests
go test -count=1 -race -v ./...
# === RUN   TestLoop_HappyPath_RestoreToDone
# --- PASS: TestLoop_HappyPath_RestoreToDone (0.07s)
# === RUN   TestLoop_CompactState_TriggersConsolidator
# ... 21 tests pass
# ok  	learn-nanobot/s11	3.5s
```

Drop `-v` and you only see the final reply — the state machine runs silently. Add `-compact-budget 100` to trigger foreground compact (if there's enough history).

Challenges:

1. Add a `/status` command to `state_command.go` that returns the session's message count and `LastConsolidated` value. Like `/help`, it should short-circuit to RESPOND and not call the LLM.
2. Insert an INJECT state after BUILD: non-blocking read one line from stdin, append it as a user message to `tc.InitialMessages`. This simulates upstream's mid-turn injection. You'll need to: (a) add `StateInject` to the `turnstate.go` enum, (b) add two edges to the Transitions map (BUILD→INJECT, INJECT→RUN), (c) write `state_inject.go`, (d) re-run `TestTransitions_AllStatesReachableFromRestore` — it should now see INJECT in the visited set.
3. Replace the `loopDispatchTrace` global with `LoopConfig.Trace func(TurnState)`. This requires changing every test's assignment to a constructor inject. It's a production-style refactor.
4. Write a real `Hook.OnStream` implementation that flushes token deltas to stdout from FinalizeContent, and wire RUN's `Verbose` to it. Remember `OnStream` has been the hook left dangling since s09 — none of s09–s11 actually invoke it.

## Upstream Source Reading / Pointers Into nanobot

Primary file: `nanobot/agent/loop.py` ([GitHub](https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/loop.py)) — 1746 lines, the longest file in the repo, and the canonical reading for s11.

You don't need to read all of it. The minimal path that hits s11's teaching points is about 200 LOC:

```
loop.py:187   TurnState enum
loop.py:208   TurnContext dataclass
loop.py:258   _TRANSITIONS table
loop.py:1372  _state_restore
loop.py:1397  _state_compact
loop.py:1402  _state_command
loop.py:1413  _state_build
loop.py:1452  _state_run
loop.py:1475  _state_save
loop.py:1501  _state_respond
```

Read each `_state_X` alongside the matching `state_X.go` in learn-Go. The diffs are mostly in three places:

1. **Media + streaming dropped.** Upstream BUILD/RUN/SAVE handle a lot of image_url / generated_media / on_stream callbacks. learn-Go punts all of that to App. B extension exercises.
2. **CommandRouter simplified.** Upstream registers commands dynamically (CronTool/SpawnTool register their own `/cron`, `/spawn`); learn-Go hardcodes `/help` and `/clear`.
3. **No runtime checkpoint.** Upstream RESTORE pulls a "pending user turn" from session metadata (for crash recovery). learn-Go assumes every turn is fresh.

Full reading notes in `upstream-readings/s11-turn-state-machine.py` — including simplified versions of three `_state_X` methods and a reading map.

LOC reference:

```
nanobot/agent/loop.py:1-77       imports + module-level constants. Skim.
nanobot/agent/loop.py:79-185     _LoopHook (in-house hook for the main loop). Skim.
nanobot/agent/loop.py:187-238    TurnState + StateTraceEntry + TurnContext. **Read carefully.**
nanobot/agent/loop.py:240-450    AgentLoop.__init__ + tool registry plumbing. Skim.
nanobot/agent/loop.py:450-1370   provider snapshot, hook plumbing, run_agent_loop.
                                 Read run_agent_loop carefully — it's the s04 Runner.Run analog.
                                 Skim everything else.
nanobot/agent/loop.py:1372-1512  the seven _state_X methods + _TRANSITIONS table. **THIS IS s11.**
nanobot/agent/loop.py:1513-1700  message persistence helpers, sanitize functions, outbound assembly.
                                 Read on a deep dive.
nanobot/agent/loop.py:1700-1746  process_direct, process_message, lock acquisition.
                                 Read when you want to understand how the bus drives the state machine.
```

Next: s_full integration chapter — read the 16-step trace and see how each step references s01-s11.
