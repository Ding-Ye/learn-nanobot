package main

import (
	"sync"
	"time"
)

// Session — a conversation thread keyed by "<channel>:<chat_id>".
// Mirrors the upstream `nanobot.session.manager.Session` dataclass, slimmed.
//
// Concurrency: a Session is touched serially by exactly one goroutine —
// the per-session goroutine the Bus spawned for it. We don't put a mutex
// on Session itself because the Bus already serializes access via its
// inbound channel (see bus.go.sessionLoop). Read-only methods like
// GetHistory are safe for the goroutine that owns the session.
//
// SessionManager has its own mutex because multiple goroutines (the Bus
// dispatcher, plus future readers like the channel adapters in s11) can
// look up sessions concurrently. The map mutation has to be guarded.
type Session struct {
	Key       string
	Messages  []Message
	CreatedAt time.Time
	UpdatedAt time.Time

	// LastConsolidated is the index of the first un-consolidated message.
	// Set to len(Messages) by the Consolidator (s10) when it summarizes a
	// prefix into the MemoryStore. Carried as a field now so s05 can
	// persist it on disk later (s06); zero today.
	LastConsolidated int
}

// SessionManager — thread-safe in-memory store of sessions, keyed by
// session key. Mirrors upstream `SessionManager` minus disk I/O (that
// arrives in s06's MemoryStore). Concurrency: the read-write mutex
// guards the sessions map only; once you hold a *Session pointer, mutation
// of that session is the caller's responsibility (in our design, the
// per-session goroutine is the sole writer).
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: map[string]*Session{}}
}

// GetOrCreate is idempotent — returns the existing Session for the key
// or constructs a new one. Mirrors upstream `SessionManager.get_or_create`.
// Safe to call from any goroutine.
func (sm *SessionManager) GetOrCreate(key string) *Session {
	// Fast path: read lock, check cache.
	sm.mu.RLock()
	if s, ok := sm.sessions[key]; ok {
		sm.mu.RUnlock()
		return s
	}
	sm.mu.RUnlock()

	// Slow path: write lock + double-check.
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if s, ok := sm.sessions[key]; ok {
		return s
	}
	s := &Session{
		Key:       key,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	sm.sessions[key] = s
	return s
}

// Get returns the session for the key without creating a new one.
// The bool indicates whether the session existed.
func (sm *SessionManager) Get(key string) (*Session, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.sessions[key]
	return s, ok
}

// Len returns the count of currently-tracked sessions. Useful for tests
// and telemetry.
func (sm *SessionManager) Len() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// Append adds a message to the session and refreshes UpdatedAt. Mirrors
// upstream `Session.add_message`. Caller must hold the per-session
// goroutine slot; we don't lock here because the Bus already serializes.
func (s *Session) Append(msg Message) {
	s.Messages = append(s.Messages, msg)
	s.UpdatedAt = time.Now()
}

// AppendAll is a convenience for appending an entire AgentRunResult.Messages
// slice — used by the Bus after Runner.Run returns.
func (s *Session) AppendAll(msgs []Message) {
	for _, m := range msgs {
		s.Messages = append(s.Messages, m)
	}
	if len(msgs) > 0 {
		s.UpdatedAt = time.Now()
	}
}

// GetHistory returns the last `maxMsgs` un-consolidated messages.
// Mirrors upstream `Session.get_history`, simplified — we drop the
// token-budget pass (s10 will reintroduce it via Consolidator's tokenizer
// hook) and the assistant-replay sanitization (anti-pattern #4 — s08's
// ContextBuilder will own that boundary).
//
// If maxMsgs <= 0 the default 120 from upstream is used.
func (s *Session) GetHistory(maxMsgs int) []Message {
	if maxMsgs <= 0 {
		maxMsgs = 120
	}
	// Slice off the consolidated prefix.
	tail := s.Messages
	if s.LastConsolidated > 0 && s.LastConsolidated <= len(s.Messages) {
		tail = s.Messages[s.LastConsolidated:]
	}
	if len(tail) <= maxMsgs {
		// Return a copy to keep the session's slice unaliased.
		out := make([]Message, len(tail))
		copy(out, tail)
		return out
	}
	out := make([]Message, maxMsgs)
	copy(out, tail[len(tail)-maxMsgs:])
	return out
}
