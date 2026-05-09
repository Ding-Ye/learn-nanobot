package main

import (
	"sync"
	"time"
)

// Session — same shape as s05/s06. Each session is a conversation thread
// keyed by "<channel>:<chat_id>". s07 leaves Session unchanged; SkillLoader
// is a separate concern (per-agent, not per-session).

type Session struct {
	Key       string
	Messages  []Message
	CreatedAt time.Time
	UpdatedAt time.Time

	// LastConsolidated — established in s06; consumed in s10. Untouched here.
	LastConsolidated int
}

// SessionManager — thread-safe in-memory store of sessions.
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

// AppendAll is a convenience for appending an entire AgentRunResult.Messages slice.
func (s *Session) AppendAll(msgs []Message) {
	for _, m := range msgs {
		s.Messages = append(s.Messages, m)
	}
	if len(msgs) > 0 {
		s.UpdatedAt = time.Now()
	}
}

// GetHistory returns the last `maxMsgs` un-consolidated messages.
func (s *Session) GetHistory(maxMsgs int) []Message {
	if maxMsgs <= 0 {
		maxMsgs = 120
	}
	tail := s.Messages
	if len(tail) <= maxMsgs {
		out := make([]Message, len(tail))
		copy(out, tail)
		return out
	}
	out := make([]Message, maxMsgs)
	copy(out, tail[len(tail)-maxMsgs:])
	return out
}
