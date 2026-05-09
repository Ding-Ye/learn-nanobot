package main

// turnstate.go — the **outer** state machine's vocabulary. Mirrors upstream
// `nanobot/agent/loop.py`: TurnState enum + TurnContext mutable bag +
// _TRANSITIONS table.
//
// Up to s10 every chapter built one component (Provider, Tool, Runner,
// Session, Memory, Skills, Context, Hook, Consolidator, AutoCompact). They
// don't talk to each other through a single conductor — main.go has been
// wiring them ad-hoc. The upstream answer is a real *outer* state machine:
//
//	RESTORE -> COMPACT -> COMMAND -> BUILD -> RUN -> SAVE -> RESPOND -> DONE
//
// with a small `_TRANSITIONS` table that says which state follows which.
// Each state owns one of the prior chapters:
//
//	RESTORE  — s05 (SessionManager) + s06 (MemoryStore)
//	COMPACT  — s10 (Consolidator)
//	COMMAND  — new in s11 (slash-command short-circuit)
//	BUILD    — s06 + s07 (Skills) + s08 (ContextBuilder)
//	RUN      — s04 (Runner) + s09 (Hook)
//	SAVE     — s06 (MemoryStore.AppendHistory)
//	RESPOND  — new in s11 (final string assembly)
//
// Notice that the *inner* tool-loop (Runner.Run; chapter s04) lives **inside**
// the RUN state. Two loops, two different jobs:
//
//   - Inner loop (Runner.Run): provider -> tool -> provider -> ... until the
//     LLM returns finish_reason="stop". This is per-iteration of the LLM.
//   - Outer loop (Loop.Process): RESTORE -> COMPACT -> ... -> DONE. This is
//     per-turn of the conversation.
//
// Both are loops; both look like `for { switch ... }` from a distance. The
// outer one is where retry/error handling, slash-command short-circuiting,
// and persistence checkpointing live. The inner one only knows about LLM
// calls and tool invocations.
//
// Why two? Because the outer concerns (lock acquisition, history slicing,
// summary file writes) are slow / IO-bound / explicit. They don't belong
// inside the tight inner loop where every additional state branch makes
// the LLM call site harder to reason about. Splitting them keeps each
// loop honest about its own responsibilities.

// TurnState is the typed enum of states the outer loop can be in. The
// numbering is `iota`-driven; the order matters for table lookup but the
// numeric values themselves are not stable across versions — never persist
// them, only persist the String() form if you must.
type TurnState int

const (
	// StateRestore is the entry state. Mirrors upstream `_state_restore`.
	// Job: get-or-create the Session via SessionManager; load any
	// pre-existing history; extract any documents/media on the inbound.
	// Reads: SessionManager (s05). Writes: Session.Messages may be
	// initialized.
	StateRestore TurnState = iota

	// StateCompact runs the Consolidator if the session's un-consolidated
	// tail is over budget. Mirrors upstream `_state_compact`. No-op for
	// short sessions; for long ones it summarizes the older half via the
	// LLM and advances Session.LastConsolidated. Reads + writes: Session.
	StateCompact

	// StateCommand checks if the inbound user text is a slash command
	// (`/help`, `/clear`). Mirrors upstream `_state_command`. If matched,
	// transitions straight to RESPOND with a canned reply. Otherwise
	// pass-through to BUILD. Reads: ctx.Msg.UserText.
	StateCommand

	// StateBuild assembles the system prompt + messages for the Runner.
	// Mirrors upstream `_state_build`. Calls into ContextBuilder (s08)
	// which itself reads MemoryStore (s06) and SkillLoader (s07). Writes:
	// tc.History, tc.SystemPrompt, tc.InitialMessages.
	StateBuild

	// StateRun is the LLM ↔ tools tight loop. Mirrors upstream `_state_run`.
	// Calls Runner.Run with the messages BUILD assembled. The Runner
	// itself (s04) iterates internally; from the outer loop's perspective
	// this is one transition. Writes: tc.Result.
	StateRun

	// StateSave persists the turn. Mirrors upstream `_state_save`. Appends
	// the user message + each assistant message in tc.Result.Messages to
	// MemoryStore.AppendHistory and updates Session.Messages. Writes:
	// memory.jsonl + session in-memory state.
	StateSave

	// StateRespond formats the final outbound. Mirrors upstream
	// `_state_respond`. Sets tc.FinalContent from the result so the Bus
	// driver in main.go can read it. Writes: tc.FinalContent.
	StateRespond

	// StateDone is the terminal success state. The driver loop checks for
	// `tc.State == StateDone` to exit.
	StateDone

	// StateError is the terminal failure state. Any state's handler that
	// returns a non-nil error sets `tc.State = StateError; tc.Err = err`
	// and the driver bails out. Mirrors upstream's behavior of bubbling
	// errors out of the state machine (rather than catching in-place).
	StateError
)

// String returns a human-readable name for the state. Used for logging
// in verbose mode and in tests asserting transition order.
//
// We deliberately match the upstream Python enum's casing
// ("RESTORE"/"COMPACT"/...) so the verbose output of `s11 -v` is
// directly comparable to `nanobot agent --debug`.
func (s TurnState) String() string {
	switch s {
	case StateRestore:
		return "RESTORE"
	case StateCompact:
		return "COMPACT"
	case StateCommand:
		return "COMMAND"
	case StateBuild:
		return "BUILD"
	case StateRun:
		return "RUN"
	case StateSave:
		return "SAVE"
	case StateRespond:
		return "RESPOND"
	case StateDone:
		return "DONE"
	case StateError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// Transitions is the canonical state graph. Keyed by the *current* state;
// value is the *next* state on the happy path. Mirrors upstream's
// `_TRANSITIONS: dict[tuple[TurnState, str], TurnState]` table — though
// upstream's keys also include an "event" string (e.g. "shortcut" jumps
// COMMAND straight to DONE). We collapse the event dimension into per-state
// methods that return the next state directly when they short-circuit
// (StateCommand returns StateRespond when it matches `/help`, etc.).
//
// Why a *map* and not a `switch` in code? Because data is inspectable.
// `state_test.go::TestTransitions_AllStatesReachableFromRestore` walks
// this map to verify every state is reachable from RESTORE. If you grow
// the state machine — add INJECT after BUILD, say — the table is the
// single source of truth and the test will tell you if you broke
// reachability. A `switch` block buries that knowledge in code paths.
//
// The map does NOT contain entries for StateDone or StateError — they're
// terminal states with no successor.
var Transitions = map[TurnState]TurnState{
	StateRestore: StateCompact,
	StateCompact: StateCommand,
	StateCommand: StateBuild,
	StateBuild:   StateRun,
	StateRun:     StateSave,
	StateSave:    StateRespond,
	StateRespond: StateDone,
}

// TurnContext is the mutable bag that flows through every state. Mirrors
// upstream `TurnContext` dataclass (loop.py:208).
//
// The pattern is *deliberate*: state methods read+write fields on this
// struct rather than returning new objects. Two reasons:
//
//  1. Composability with Go's `for { switch ... }` driver pattern. If
//     each method returned a new context, the driver would have to
//     re-thread every field through every step. With a mutable bag
//     the driver hands the same pointer to each handler.
//
//  2. Mirrors upstream's Python pattern (also a mutable dataclass). When
//     a learner is reading `loop.py` alongside this file the field
//     access patterns line up exactly — `ctx.session = ...` in Python
//     reads the same as `tc.Session = ...` in Go.
//
// The cost is that handlers can leave the bag in an inconsistent state
// if they error out mid-write. We mitigate by making each handler
// transactional: it either fully populates its outputs OR sets State =
// StateError + Err. The driver loop checks State on every iteration so
// a partially-mutated bag still terminates cleanly.
type TurnContext struct {
	// Msg is the inbound message that initiated this turn. Set by the
	// caller before Process() runs and never mutated by the state
	// machine.
	Msg InboundMessage

	// SessionKey is the conversation thread identifier (e.g.
	// "cli:direct"). Pulled from Msg.SessionKey at the top of Process().
	SessionKey string

	// State is the current position in the state graph. Drives the
	// `for { switch tc.State }` loop in Process().
	State TurnState

	// Session is the in-memory Session record (history, cursor, etc.).
	// Populated by StateRestore via SessionManager.GetOrCreate. Used by
	// every subsequent state.
	Session *Session

	// History is the trimmed slice of prior messages used to seed the
	// Runner. Populated by StateBuild from Session.GetHistory + token
	// budget; consumed by StateRun.
	History []Message

	// SystemPrompt is the assembled system prompt the Runner passes
	// through to the Provider. Populated by StateBuild via
	// ContextBuilder.BuildSystemPrompt; consumed by StateRun.
	SystemPrompt string

	// InitialMessages is the messages slice (history + current user
	// message) that StateBuild assembled and StateRun feeds to
	// Runner.Run.
	InitialMessages []Message

	// Result is the AgentRunResult produced by Runner.Run inside
	// StateRun. Populated by StateRun; consumed by StateSave and
	// StateRespond.
	Result *AgentRunResult

	// FinalContent is the user-facing text response. Set by StateRespond
	// (or by StateCommand on a slash-command short-circuit). The Bus
	// driver in main.go reads this after Process() returns.
	FinalContent string

	// ToolsUsed is the deduplicated list of tool names this turn
	// invoked. Populated by StateSave from Result.ToolsUsed. Surfaced
	// for the SDK observation surface.
	ToolsUsed []string

	// Generated tracks media/files this turn produced (image_url paths,
	// generated artifacts). Field is reserved for future expansion;
	// today it stays empty in the curriculum's CLI build.
	Generated []string

	// Err is the terminal error if State == StateError. The driver
	// returns this from Process() so callers can wrap it for telemetry
	// or surface it through the Bus's OutboundMessage.Err.
	Err error
}

// IsTerminal returns true when the state machine has reached an exit
// state and the driver loop should stop. Used by Process()'s loop
// condition; tests use it to assert the machine actually terminated.
func (tc *TurnContext) IsTerminal() bool {
	return tc.State == StateDone || tc.State == StateError
}

// fail sets the terminal error fields. Centralized so handlers don't
// have to remember to set both `Err` and `State` (a common bug source
// in upstream's Python equivalent before the dataclass was tightened).
func (tc *TurnContext) fail(err error) {
	tc.Err = err
	tc.State = StateError
}

// advance moves the context to the next state via the Transitions table.
// Returns false if no transition is defined (i.e. the state is terminal).
// Centralizing the lookup means `state_test.go` can walk the same code
// path the driver uses — the table is the only source of truth.
func (tc *TurnContext) advance() bool {
	next, ok := Transitions[tc.State]
	if !ok {
		return false
	}
	tc.State = next
	return true
}
