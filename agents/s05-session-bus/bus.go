package main

import (
	"context"
	"fmt"
	"sync"
)

// Bus — the message bus that decouples "where the message came from"
// (CLI, Telegram, Slack, …) from "the agent that processes it".
// Mirrors upstream `nanobot.bus.queue.MessageBus`, but does more work in
// one place: this Bus also owns the per-session goroutine that turns
// inbound messages into Runner.Run calls. Upstream splits that across
// MessageBus + AgentLoop (s11 will do the same split).
//
// Concurrency model:
//
//   - One inbound chan per session, keyed by SessionKey.
//   - One goroutine per session, started lazily on first Send.
//   - All session goroutines share one outbound chan (the Bus.out).
//
// This is the Go-flavored translation of upstream's per-session
// asyncio.Lock: Python uses `async with self._locks[key]` to serialize
// turns within a session; we get the same property by structure — only
// one goroutine reads from the session's inbound chan, so its Runner.Run
// calls happen one-at-a-time. Across sessions the goroutines run
// concurrently — exactly what we want.
type Bus struct {
	sessions *SessionManager
	runner   *Runner
	tools    *Registry

	// Per-Bus run config — a real product wires these per-session, but
	// for s05 a single (model, maxIter) is enough to keep the focus on
	// the dispatch+goroutine machinery.
	model   string
	maxIter int

	mu   sync.Mutex                  // guards in + done
	in   map[string]chan InboundMessage
	out  chan OutboundMessage
	wg   sync.WaitGroup
	done chan struct{} // closed by Stop to tell session loops to drain+exit
}

// InboundMessage is a request entering the bus. Mirrors
// `nanobot.bus.events.InboundMessage` minus channel-specific fields.
// SessionKey is the logical conversation id (e.g. "telegram:42",
// "slack:C123", "cli:direct"); the Bus uses it to look up or spawn a
// session goroutine.
type InboundMessage struct {
	SessionKey string
	UserText   string

	// ID is an optional per-message identifier; OutboundMessage.CorrelationID
	// will echo it so callers can match request to reply. Tests use this to
	// assert serial ordering inside one session.
	ID string
}

// OutboundMessage is the agent's reply, ready for whatever channel sent
// the inbound. Err is non-nil if Runner.Run failed; Text is the assistant's
// final reply otherwise. Mirrors upstream OutboundMessage minus channel
// fields.
type OutboundMessage struct {
	SessionKey    string
	Text          string
	Err           error
	CorrelationID string
}

// NewBus wires the Bus to its dependencies. The Runner and Registry are
// shared across all session goroutines (Runner is stateless; Registry is
// effectively immutable after startup, with its mutation paths protected
// by callers).
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
// spawning one on first use. Send is non-blocking unless the session's
// inbound chan is full (cap 8) — that backpressure is intentional: it
// keeps a runaway producer from filling unbounded memory.
//
// Calling Send after Stop returns a "bus stopped" outbound on the out
// chan — we don't enqueue to a closed inbound. Tests rely on this.
func (b *Bus) Send(msg InboundMessage) {
	b.mu.Lock()
	select {
	case <-b.done:
		// Bus is stopped — drop on the floor with a typed outbound so
		// callers blocked on Out() see something rather than hanging.
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
		// First message for this session — create chan and spawn the
		// session goroutine. The goroutine's lifetime is bounded by
		// Stop(): we close the chan to signal it to drain and exit.
		ch = make(chan InboundMessage, 8)
		b.in[msg.SessionKey] = ch
		b.wg.Add(1)
		go b.sessionLoop(msg.SessionKey, ch)
	}
	b.mu.Unlock()

	// Send outside the lock so a slow consumer can't block dispatch
	// of OTHER sessions. If THIS session's chan is full, we block — that's
	// the point of bounded backpressure.
	ch <- msg
}

// Out returns the outbound channel the bus writes replies to. The caller
// (main, or s11's channel adapters) reads OutboundMessages off this chan.
func (b *Bus) Out() <-chan OutboundMessage { return b.out }

// Stop closes all per-session inbound channels and waits for every
// session goroutine to drain its remaining messages and exit. Safe to
// call once. After Stop returns, the out channel is closed.
//
// Pending messages in a session's inbound chan are processed before the
// loop exits — TestBus_StopBeforeProcessing_Drains relies on this.
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

// sessionLoop is the per-session goroutine. Reads inbound messages from
// `ch` until the channel is closed (Stop); for each one, looks up the
// session, runs the Runner with the session's history + the new user
// turn, then appends the result to the session and emits an
// OutboundMessage.
//
// Errors from Runner.Run are surfaced as OutboundMessage.Err — the caller
// decides what to do. We don't crash the goroutine on transient errors
// because the next inbound message in the queue might still be valid.
func (b *Bus) sessionLoop(key string, ch <-chan InboundMessage) {
	defer b.wg.Done()
	ctx := context.Background()

	for msg := range ch {
		sess := b.sessions.GetOrCreate(key)
		// Build the messages: existing history + the new user turn.
		history := sess.GetHistory(0) // 0 ⇒ default cap of 120 (upstream)
		userMsg := Message{
			Role: "user",
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

		// Persist the new user turn + everything Runner appended.
		sess.Append(userMsg)
		// result.Messages contains the FULL message log including the
		// initial messages. We just want the assistant turns produced by
		// the Runner, which are the slice past `len(initial)`.
		if len(result.Messages) > len(initial) {
			sess.AppendAll(result.Messages[len(initial):])
		}

		out := OutboundMessage{
			SessionKey:    key,
			Text:          result.FinalContent,
			Err:           err,
			CorrelationID: msg.ID,
		}
		b.out <- out
	}
}
