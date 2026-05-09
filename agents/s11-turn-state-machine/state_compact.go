package main

import (
	"context"
	"errors"
	"fmt"
)

// stateCompact is the foreground consolidation handler. Mirrors upstream
// `_state_compact` (loop.py:1397).
//
// Job: if the session's un-consolidated tail estimate exceeds the
// configured budget, call Consolidator.Consolidate to summarize the
// older half before we hand the messages to the Runner. Otherwise
// no-op.
//
// Why foreground? Two reasons:
//
//  1. **Determinism for the current turn.** AutoCompact (s10) runs in a
//     background goroutine on a TTL ticker. That's great for sessions
//     that are *idle* — compress them while nobody's looking. But if a
//     session is mid-turn and has just blown past the budget, we need
//     consolidation to happen *now*, before the Runner gets the
//     messages, or else this turn's prompt will be too big for the
//     model.
//
//  2. **Synchronization with the bus lock.** The per-session goroutine
//     holds the only writer pointer to Session.Messages; running the
//     consolidator inline means we don't have to coordinate with
//     AutoCompact (which checks `archiving[key]` to avoid double-firing).
//
// If `l.CompactBudget` is zero (the curriculum default), this state is
// a pure no-op: the AutoCompact goroutine handles compaction
// out-of-band. Set CompactBudget to the same value as the inner-loop
// context window to compact aggressively in the foreground.
//
// Boundary not found:
//
// Consolidator.Consolidate returns ErrNoLegalBoundary when the session
// is too short to find a clean user-turn cut point. That's a normal
// "nothing to do" outcome — we treat it as success here. The session
// will retry compaction on the next turn (or when AutoCompact picks it
// up after the TTL).
func (l *Loop) stateCompact(ctx context.Context, tc *TurnContext) error {
	if tc.Session == nil {
		// Defensive: stateRestore should always populate this.
		return errors.New("stateCompact: tc.Session is nil")
	}
	if l.Consolidator == nil || l.CompactBudget <= 0 {
		// Compaction not configured for this Loop — silent no-op. The
		// AutoCompact goroutine (if wired) will handle it later.
		return nil
	}

	// Estimate the un-consolidated tail. If under budget, we're fine.
	tail := 0
	for i := tc.Session.LastConsolidated; i < len(tc.Session.Messages); i++ {
		tail += l.Consolidator.estimateMessageTokens(tc.Session.Messages[i])
	}
	if tail <= l.CompactBudget {
		return nil
	}

	if l.Verbose {
		fmt.Printf("[s11] compact: session=%q tail=%d budget=%d -> consolidating\n",
			tc.SessionKey, tail, l.CompactBudget)
	}

	if _, err := l.Consolidator.Consolidate(ctx, tc.Session); err != nil {
		// ErrNoLegalBoundary is normal — treat as no-op rather than
		// fail the whole turn.
		if errors.Is(err, ErrNoLegalBoundary) {
			if l.Verbose {
				fmt.Printf("[s11] compact: no legal boundary for %q; skipping\n", tc.SessionKey)
			}
			return nil
		}
		return fmt.Errorf("consolidate: %w", err)
	}
	return nil
}
