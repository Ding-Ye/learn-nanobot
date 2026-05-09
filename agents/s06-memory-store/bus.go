package main

import (
	"context"
	"fmt"
	"sync"
)

// Bus — the message bus that decouples "where the message came from" from
// "the agent that processes it". Same shape as s05; carried over verbatim
// because each session is its own self-contained Go module.
//
// What changed in s06: the bus's session goroutine now persists the new
// turn to a `MemoryStore` (when one is wired). The Bus type itself is the
// same — main.go is the place where MemoryStore.AppendHistory gets called.
// We keep the "Bus is just dispatch" rule by leaving persistence in main:
// future sessions (s11) will move it into the TurnState.SAVE state.
type Bus struct {
	sessions *SessionManager
	runner   *Runner
	tools    *Registry

	model   string
	maxIter int

	mu   sync.Mutex
	in   map[string]chan InboundMessage
	out  chan OutboundMessage
	wg   sync.WaitGroup
	done chan struct{}
}

// InboundMessage is a request entering the bus.
type InboundMessage struct {
	SessionKey string
	UserText   string
	ID         string
}

// OutboundMessage is the agent's reply, ready for whatever channel sent
// the inbound. Result carries the full AgentRunResult so main can pull
// out usage / tools_used / messages for persistence to MemoryStore.
type OutboundMessage struct {
	SessionKey    string
	Text          string
	Err           error
	CorrelationID string

	// Result is the full Runner output. main.go reads ToolsUsed and Usage
	// from here to feed MemoryStore.AppendHistory. It's the cheapest
	// extension point — adding it doesn't change the s05 channel shape
	// (Text/Err/SessionKey/CorrelationID), it just exposes more.
	Result AgentRunResult
}

// NewBus wires the Bus to its dependencies.
func NewBus(sm *SessionManager, r *Runner, tools *Registry, model string, maxIter int) *Bus {
	return &Bus{
		sessions: sm,
		runner:   r,
		tools:    tools,
		model:    model,
		maxIter:  maxIter,
		in:       map[string]chan InboundMessage{},
		out:      make(chan OutboundMessage, 64),
		done:     make(chan struct{}),
	}
}

// Send dispatches an inbound message to the right session goroutine,
// spawning one on first use.
func (b *Bus) Send(msg InboundMessage) {
	b.mu.Lock()
	select {
	case <-b.done:
		b.mu.Unlock()
		go func() {
			b.out <- OutboundMessage{
				SessionKey:    msg.SessionKey,
				Err:           fmt.Errorf("bus: stopped, dropping message %q", msg.ID),
				CorrelationID: msg.ID,
			}
		}()
		return
	default:
	}

	ch, ok := b.in[msg.SessionKey]
	if !ok {
		ch = make(chan InboundMessage, 8)
		b.in[msg.SessionKey] = ch
		b.wg.Add(1)
		go b.sessionLoop(msg.SessionKey, ch)
	}
	b.mu.Unlock()

	ch <- msg
}

// Out returns the outbound channel.
func (b *Bus) Out() <-chan OutboundMessage { return b.out }

// Stop closes all per-session inbound channels and waits for every
// session goroutine to drain its remaining messages and exit.
func (b *Bus) Stop() {
	b.mu.Lock()
	select {
	case <-b.done:
		b.mu.Unlock()
		return
	default:
	}
	close(b.done)
	for _, ch := range b.in {
		close(ch)
	}
	b.mu.Unlock()
	b.wg.Wait()
	close(b.out)
}

// sessionLoop is the per-session goroutine.
func (b *Bus) sessionLoop(key string, ch <-chan InboundMessage) {
	defer b.wg.Done()
	ctx := context.Background()

	for msg := range ch {
		sess := b.sessions.GetOrCreate(key)
		history := sess.GetHistory(0)
		userMsg := Message{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: msg.UserText}},
		}
		initial := append(history, userMsg)

		spec := AgentRunSpec{
			InitialMessages: initial,
			Tools:           b.tools,
			Model:           b.model,
			MaxIterations:   b.maxIter,
		}

		result, err := b.runner.Run(ctx, spec)

		sess.Append(userMsg)
		if len(result.Messages) > len(initial) {
			sess.AppendAll(result.Messages[len(initial):])
		}

		out := OutboundMessage{
			SessionKey:    key,
			Text:          result.FinalContent,
			Err:           err,
			CorrelationID: msg.ID,
			Result:        result,
		}
		b.out <- out
	}
}
