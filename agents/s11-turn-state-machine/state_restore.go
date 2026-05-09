package main

import (
	"context"
	"errors"
	"fmt"
)

// stateRestore is the entry handler for the outer state machine. Mirrors
// upstream `_state_restore` (loop.py:1372).
//
// Job:
//
//  1. Resolve the Session via SessionManager.GetOrCreate. The session
//     might already exist (if this is the second turn of an ongoing
//     conversation) or be brand new (first turn after agent boot).
//
//  2. Stash the session pointer on tc.Session so every subsequent state
//     can reach it without re-doing the lookup.
//
// Upstream's version also restores a "runtime checkpoint" and "pending
// user turn" from disk (the agent might have crashed mid-turn and need
// to resume). We omit those — for the curriculum a fresh session lookup
// is enough to teach the pattern, and crash-recovery would distract
// from the state-machine teaching point.
//
// Errors:
//
//   - SessionKey is empty — programming error in the caller; we surface
//     it immediately rather than letting StateCompact stumble on a nil
//     session.
//   - SessionManager is nil — same.
//
// Both errors transition the state machine to StateError; the driver
// loop bails out and Process() returns the wrapped error.
func (l *Loop) stateRestore(ctx context.Context, tc *TurnContext) error {
	_ = ctx // reserved for future cancellation; SessionManager is sync today

	if tc.SessionKey == "" {
		return errors.New("stateRestore: SessionKey must be non-empty")
	}
	if l.Sessions == nil {
		return errors.New("stateRestore: Sessions must be set")
	}

	tc.Session = l.Sessions.GetOrCreate(tc.SessionKey)
	if tc.Session == nil {
		// SessionManager.GetOrCreate is supposed to be idempotent and
		// always return non-nil. If it didn't, something is very wrong
		// — surface immediately.
		return fmt.Errorf("stateRestore: GetOrCreate(%q) returned nil", tc.SessionKey)
	}

	return nil
}
