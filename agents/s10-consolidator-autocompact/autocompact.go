package main

import (
	"context"
	"sync"
	"time"
)

// AutoCompact — TTL-based background consolidator. Mirrors upstream
// `nanobot/agent/autocompact.py::AutoCompact`.
//
// The job: periodically scan SessionManager.sessions; for any session
// that is (a) not currently active in a turn and (b) has been idle
// longer than the configured TTL, spawn a goroutine that calls
// Consolidator.Consolidate. The point is to compress dormant
// conversations *before* the user comes back so the next turn starts
// from a smaller context window.
//
// Three invariants the implementation has to keep:
//
//  1. **Fire-and-forget.** AutoCompact does NOT block the agent loop.
//     It spawns goroutines and trusts the consolidator to either
//     succeed or surface its own error. The CheckExpired return value
//     is a list of session keys that were *queued*, not a list of
//     session keys that finished.
//
//  2. **Idempotent within a tick.** Two consecutive CheckExpired calls
//     in quick succession must not spawn the same consolidation
//     twice. We track in-flight consolidations in `archiving` and
//     skip any session that's already there. Mirrors upstream's
//     `_archiving: set[str]`.
//
//  3. **Active sessions are sacred.** A session being actively
//     processed by the Bus must NOT be consolidated mid-turn —
//     that would race the bus goroutine on `Session.Messages`. The
//     caller passes in `activeSessionKeys` (a snapshot of the keys
//     currently held by the Bus) and AutoCompact filters them out.

// AutoCompact wraps a SessionManager + Consolidator and runs a TTL
// ticker on demand.
type AutoCompact struct {
	// Sessions is the SessionManager whose sessions[] map we scan.
	Sessions *SessionManager

	// Consolidator is the worker that actually runs the LLM
	// summarization and persists the result. AutoCompact is just the
	// scheduler.
	Consolidator *Consolidator

	// TTL is the idle threshold: if `time.Since(s.UpdatedAt) > TTL`,
	// the session is eligible for compaction. Mirrors upstream's
	// `session_ttl_minutes`.
	TTL time.Duration

	mu        sync.Mutex
	archiving map[string]bool
	summaries map[string]string
}

// NewAutoCompact constructs an AutoCompact wired to the given session
// manager and consolidator. TTL is the idle threshold; pass 0 to
// disable expiration entirely (`CheckExpired` becomes a no-op).
func NewAutoCompact(sm *SessionManager, c *Consolidator, ttl time.Duration) *AutoCompact {
	return &AutoCompact{
		Sessions:     sm,
		Consolidator: c,
		TTL:          ttl,
		archiving:    map[string]bool{},
		summaries:    map[string]string{},
	}
}

// CheckExpired scans every session in the manager and queues a
// consolidation goroutine for any session that is (1) idle longer
// than TTL, (2) not currently active, and (3) not already being
// consolidated. Returns the list of session keys that were queued
// in this call.
//
// activeSessionKeys is typically `bus.ActiveKeys()` or some equivalent
// snapshot owned by the caller. AutoCompact does not subscribe to the
// bus directly because cross-package coupling there would force a
// circular import; instead the caller passes the snapshot per call.
func (a *AutoCompact) CheckExpired(ctx context.Context, activeSessionKeys map[string]bool) []string {
	if a == nil || a.TTL <= 0 || a.Sessions == nil {
		return nil
	}

	now := time.Now()
	var queued []string

	a.mu.Lock()
	defer a.mu.Unlock()

	a.Sessions.mu.RLock()
	keys := make([]string, 0, len(a.Sessions.sessions))
	for k := range a.Sessions.sessions {
		keys = append(keys, k)
	}
	a.Sessions.mu.RUnlock()

	for _, key := range keys {
		if activeSessionKeys != nil && activeSessionKeys[key] {
			continue
		}
		if a.archiving[key] {
			continue
		}
		sess, ok := a.Sessions.Get(key)
		if !ok {
			continue
		}
		if now.Sub(sess.UpdatedAt) <= a.TTL {
			continue
		}
		// Mark as archiving BEFORE spawning so a second CheckExpired
		// in flight can't queue the same key twice.
		a.archiving[key] = true
		queued = append(queued, key)
		go a.runOne(ctx, sess)
	}

	return queued
}

// runOne is the goroutine body. It runs the Consolidator, captures the
// summary into the in-memory cache, and clears the archiving flag in a
// deferred block so a panic doesn't leave the session permanently
// blocked.
func (a *AutoCompact) runOne(ctx context.Context, sess *Session) {
	defer func() {
		a.mu.Lock()
		delete(a.archiving, sess.Key)
		a.mu.Unlock()
	}()

	if a.Consolidator == nil {
		return
	}

	summary, err := a.Consolidator.Consolidate(ctx, sess)
	if err != nil {
		// Errors (including ErrNoLegalBoundary, which is a normal
		// "nothing to do" case) are absorbed silently — the next
		// tick will retry if the session is still expired.
		return
	}

	a.mu.Lock()
	a.summaries[sess.Key] = summary
	a.mu.Unlock()
}

// LastSummary returns the most recently captured summary for the given
// session key (empty string if none). Used by main.go's verbose mode
// and by tests that want to assert "the goroutine completed".
func (a *AutoCompact) LastSummary(key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.summaries[key]
}

// IsArchiving reports whether a consolidation goroutine is currently
// in flight for the given session key. Tests use this to wait for the
// goroutine to finish.
func (a *AutoCompact) IsArchiving(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.archiving[key]
}

// Run starts the periodic ticker loop. Calls CheckExpired every
// `interval` until ctx is cancelled. `activeKeys` is a function
// callback so the snapshot can be refreshed per tick (the bus's
// active-key set changes over time).
//
// Run blocks; production code spawns it inside a goroutine like:
//
//	go autoCompact.Run(ctx, 30*time.Second, bus.ActiveKeys)
//
// Cancelling ctx (or returning the empty map twice in a row, which
// just means "no work this tick") is the only way to stop the loop.
func (a *AutoCompact) Run(ctx context.Context, interval time.Duration, activeKeys func() map[string]bool) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var snap map[string]bool
			if activeKeys != nil {
				snap = activeKeys()
			}
			a.CheckExpired(ctx, snap)
		}
	}
}
