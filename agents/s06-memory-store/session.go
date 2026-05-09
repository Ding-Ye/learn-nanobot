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
// inbound channel (see bus.go.sessionLoop).
type Session struct {
	Key       string
	Messages  []Message
	CreatedAt time.Time
	UpdatedAt time.Time

	// LastConsolidated is the cursor in `history.jsonl` corresponding to
	// the last entry the Consolidator has summarized into MEMORY.md.
	// Anything with a higher cursor is "fresh" history that still belongs
	// in the model's context window.
	//
	// In s05 this field existed but was dead — there was no on-disk log to
	// be consolidated. In s06 it gets filled in for real: every successful
	// AppendHistory writes a new cursor to the .cursor file, and Session
	// records the latest value here so future consolidation (s10) can
	// compare "last consolidated cursor" against "current cursor on disk"
	// to know how much new material exists.
	//
	// filled in for real in s06; consumed in s10.
	LastConsolidated int
}

// SessionManager — thread-safe in-memory store of sessions, keyed by
// session key. Mirrors upstream `SessionManager` minus disk I/O of the
// session's full message log. The MemoryStore added in s06 owns the disk
// half: history.jsonl + MEMORY.md/SOUL.md/USER.md. SessionManager itself
// stays in-memory because each session goroutine still needs a hot
// in-memory message log to drive `Runner.Run`.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewSessionManager() *SessionManager {
	return &SessionManager{sessions: map[string]*Session{}}
}

// GetOrCreate is idempotent — returns the existing Session for the key
// or constructs a new one.
func (sm *SessionManager) GetOrCreate(key string) *Session {
	sm.mu.RLock()
	if s, ok := sm.sessions[key]; ok {
		sm.mu.RUnlock()
		return s
	}
	sm.mu.RUnlock()

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
func (sm *SessionManager) Get(key string) (*Session, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	s, ok := sm.sessions[key]
	return s, ok
}

// Len returns the count of currently-tracked sessions.
func (sm *SessionManager) Len() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// Append adds a message to the session and refreshes UpdatedAt. The
// per-session Bus goroutine is the sole writer; no lock here.
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

// GetHistory returns the last `maxMsgs` un-consolidated messages, mirroring
// upstream `Session.get_history` (slimmed).
func (s *Session) GetHistory(maxMsgs int) []Message {
	if maxMsgs <= 0 {
		maxMsgs = 120
	}
	tail := s.Messages
	// Note: LastConsolidated is now an on-disk cursor (s06), not an index
	// into Messages. We keep the in-memory tail-slicing behavior simple —
	// s10 will reconcile the two ideas. For now LastConsolidated > 0 means
	// "some prefix has already been summarized into MEMORY.md", and the
	// in-memory log is allowed to keep all messages (the live context is
	// still the slice past `len(Messages)-maxMsgs`).
	if len(tail) <= maxMsgs {
		out := make([]Message, len(tail))
		copy(out, tail)
		return out
	}
	out := make([]Message, maxMsgs)
	copy(out, tail[len(tail)-maxMsgs:])
	return out
}
