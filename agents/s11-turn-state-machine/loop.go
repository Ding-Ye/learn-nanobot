package main

import (
	"context"
	"fmt"
)

// Loop — the **outer** state machine. Mirrors upstream
// `nanobot/agent/loop.py::AgentLoop`.
//
// IMPORTANT: this `Loop` is NOT the same type as s01's `Loop`. They share
// a name and that's it. Each session in this curriculum is its own Go
// module, so s01.Loop and s11.Loop are *literally different types* by
// Go's package-scoping rules — there's no conflict and no confusion at
// compile time.
//
// Why reuse the name? Two reasons:
//
//  1. **Upstream uses it.** `nanobot/agent/loop.py::AgentLoop` is the
//     name of the canonical state machine in the upstream codebase. We
//     mirror it so the file-to-file teaching mapping ("read s11/loop.go
//     while reading nanobot/agent/loop.py") is exact.
//
//  2. **It's the natural name.** Earlier sessions used `Loop` for the
//     simplest thing they had — by s11 we own the conductor and the
//     name belongs back to it.
//
// The shape is entirely different from s01:
//
//	s01: type Loop struct { Provider Provider; Tools *Registry; ... }
//	s11: type Loop struct { Runner *Runner; CtxBuilder *ContextBuilder;
//	                        Sessions *SessionManager; Memory *MemoryStore;
//	                        Bus *Bus; Consolidator *Consolidator;
//	                        Hook Hook; Model string; MaxIter int }
//
// s11's Loop wraps EIGHT components, one of which (RUN) wraps the LLM
// via Runner. Reading this file top-to-bottom is the closest learn-nanobot
// gets to reading nanobot itself.

// Loop is the outer state machine. One Loop per agent. Process() is the
// entry point — call it once per inbound message; it drives the state
// graph from RESTORE through DONE (or ERROR).
type Loop struct {
	// Runner is the inner LLM ↔ tools loop (s04). Lives inside StateRun.
	Runner *Runner

	// Tools is the registry of executable tools (s02). Passed to
	// Runner.Run via AgentRunSpec.Tools. Held by Loop (rather than by
	// Runner) so that swapping tool sets per-Loop is one field change.
	Tools *Registry

	// CtxBuilder assembles the system prompt + messages for the Runner
	// (s08). Lives inside StateBuild.
	CtxBuilder *ContextBuilder

	// Sessions is the in-memory session manager (s05). StateRestore
	// calls GetOrCreate; StateSave reads/writes Session.Messages.
	Sessions *SessionManager

	// Memory is the file-backed memory store (s06). StateBuild reads
	// MEMORY.md / SOUL.md / USER.md / TOOLS.md through it; StateSave
	// appends turn entries to memory.jsonl.
	Memory *MemoryStore

	// Bus is the message bus (s05). The Loop is conceptually owned by
	// the Bus — the per-session goroutine in Bus.sessionLoop calls
	// Loop.Process. The reference is here for completeness and for
	// future extensions (e.g. a state could enqueue a side-message via
	// bus.Send), but Process() does NOT call into Bus today.
	Bus *Bus

	// Consolidator is the memory-compression pipeline (s10). StateCompact
	// calls Consolidate when the session's un-consolidated tail is over
	// budget. Optional — if nil, StateCompact is a no-op.
	Consolidator *Consolidator

	// Hook is the lifecycle observability surface (s09). Threaded through
	// to the Runner's Hook field (Runner.Hook is set at construction).
	// Available here in case future states want to fire their own taps
	// (none do today).
	Hook Hook

	// Model is the LLM model id passed to Runner.Run. Pulled from
	// configuration; defaults to "claude-sonnet-4-6" matching the
	// rest of the curriculum.
	Model string

	// MaxIter is the inner-loop iteration cap passed to AgentRunSpec.
	// Mirrors upstream's `max_iterations` default. Setting this too low
	// is the most common bug — the LLM might need 5-10 tool round-trips
	// for a non-trivial query.
	MaxIter int

	// CompactBudget — token threshold above which StateCompact triggers
	// the Consolidator. If a session's un-consolidated tail estimate
	// exceeds this, we kick off consolidation. Zero disables compaction
	// in the foreground (AutoCompact still runs in the background).
	CompactBudget int

	// Verbose — when true, Process() prints state transitions to stdout
	// in the format "[s11] state: RESTORE -> COMPACT". Useful for
	// teaching and for confirming the state graph is being walked
	// correctly.
	Verbose bool
}

// LoopConfig is an ergonomic builder. Fields default to sensible values
// when left zero — see NewLoop's defaulting logic. Most callers will set
// Runner, Tools, CtxBuilder, Sessions, Memory and accept the rest.
type LoopConfig struct {
	Runner        *Runner
	Tools         *Registry
	CtxBuilder    *ContextBuilder
	Sessions      *SessionManager
	Memory        *MemoryStore
	Bus           *Bus
	Consolidator  *Consolidator
	Hook          Hook
	Model         string
	MaxIter       int
	CompactBudget int
	Verbose       bool
}

// NewLoop constructs a Loop from a config. Required fields: Runner,
// CtxBuilder, Sessions, Memory. Other fields default — Hook becomes
// NoopHook, Model becomes "claude-sonnet-4-6", MaxIter becomes 20.
func NewLoop(cfg LoopConfig) *Loop {
	if cfg.Hook == nil {
		cfg.Hook = NoopHook{}
	}
	if cfg.Model == "" {
		cfg.Model = "claude-sonnet-4-6"
	}
	if cfg.MaxIter <= 0 {
		cfg.MaxIter = 20
	}
	return &Loop{
		Runner:        cfg.Runner,
		Tools:         cfg.Tools,
		CtxBuilder:    cfg.CtxBuilder,
		Sessions:      cfg.Sessions,
		Memory:        cfg.Memory,
		Bus:           cfg.Bus,
		Consolidator:  cfg.Consolidator,
		Hook:          cfg.Hook,
		Model:         cfg.Model,
		MaxIter:       cfg.MaxIter,
		CompactBudget: cfg.CompactBudget,
		Verbose:       cfg.Verbose,
	}
}

// Process is the state machine driver. It seeds a TurnContext from the
// inbound message, then walks the state graph until a terminal state
// (DONE or ERROR) is reached. The TurnContext is returned so callers can
// inspect FinalContent, ToolsUsed, the resulting Result, and the trace.
//
// The driver shape — `for !tc.IsTerminal() { switch tc.State { ... } }` —
// mirrors upstream's `_process_turn` method (loop.py:1213). Each switch
// branch calls into a single state method which mutates tc and either:
//
//   - returns nil (success) → driver advances to the next state via
//     Transitions[tc.State]
//   - returns non-nil error → driver sets tc.State = StateError, tc.Err
//     and exits
//   - sets tc.State directly (e.g. StateCommand jumps to StateRespond
//     on slash-command match) → driver respects that and does NOT
//     consult Transitions
//
// The third option — handlers writing to tc.State directly — is what
// lets StateCommand short-circuit. Without it we'd need an "event" key
// in the Transitions table (which is what upstream does in its slightly
// fuller version — see loop.py:258-267). For the curriculum's purposes
// the simpler "handler can override state" pattern teaches the same idea
// with less ceremony.
func (l *Loop) Process(ctx context.Context, msg InboundMessage) (*TurnContext, error) {
	tc := &TurnContext{
		Msg:        msg,
		SessionKey: msg.SessionKey,
		State:      StateRestore,
	}

	if l.Verbose {
		fmt.Printf("[s11] state: START -> %s\n", tc.State)
	}

	for !tc.IsTerminal() {
		prev := tc.State
		if loopDispatchTrace != nil {
			loopDispatchTrace(prev)
		}
		err := l.dispatch(ctx, tc)

		if err != nil {
			tc.fail(fmt.Errorf("%s: %w", prev, err))
			if l.Verbose {
				fmt.Printf("[s11] state: %s -> ERROR (%v)\n", prev, err)
			}
			break
		}

		// Handlers may write tc.State directly (e.g. StateCommand jumps
		// to StateRespond on slash-command match). If they did, respect
		// it; otherwise consult the Transitions table for the next state.
		if tc.State == prev {
			if !tc.advance() {
				// No transition defined and the state didn't terminate.
				// Treat as a programming error — surfaces a missing
				// Transitions entry early instead of hanging.
				tc.fail(fmt.Errorf("no transition from %s", prev))
				break
			}
		}

		if l.Verbose {
			fmt.Printf("[s11] state: %s -> %s\n", prev, tc.State)
		}
	}

	if tc.State == StateError {
		return tc, tc.Err
	}
	return tc, nil
}

// loopDispatchTrace is a test-only hook. When non-nil it's invoked once
// per state-machine tick with the *current* state (before dispatch). Tests
// in loop_test.go assign it to record the visited sequence and then
// reset it. Production callers leave it nil (the cost is one nil-check
// per tick).
//
// Why a package-level var? Because the alternative — threading a "trace"
// callback through LoopConfig — bloats the public surface for a teaching
// hook that nobody outside tests will ever set. We accept the small
// trade-off (a global mutated by tests, with the obvious caveat that
// parallel tests must not collide on it; today's test suite is sequential).
var loopDispatchTrace func(TurnState)

// dispatch is the single switch over states. Each state has a dedicated
// method in state_<name>.go to keep this file from growing unbounded.
func (l *Loop) dispatch(ctx context.Context, tc *TurnContext) error {
	switch tc.State {
	case StateRestore:
		return l.stateRestore(ctx, tc)
	case StateCompact:
		return l.stateCompact(ctx, tc)
	case StateCommand:
		return l.stateCommand(ctx, tc)
	case StateBuild:
		return l.stateBuild(ctx, tc)
	case StateRun:
		return l.stateRun(ctx, tc)
	case StateSave:
		return l.stateSave(ctx, tc)
	case StateRespond:
		return l.stateRespond(ctx, tc)
	case StateDone, StateError:
		// Terminal — caller should have exited the loop. Reaching here
		// means the loop condition is buggy; fail loudly.
		return fmt.Errorf("dispatch: terminal state %s reached unexpectedly", tc.State)
	default:
		return fmt.Errorf("dispatch: unknown state %s", tc.State)
	}
}
