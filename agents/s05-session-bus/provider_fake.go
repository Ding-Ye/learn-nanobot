package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// FakeProvider is the deterministic test double established in s03.
// Tests script a list of LLMResponses; FakeProvider returns them in order.
// No network, no API key. Reused unchanged here in s05 — except we wrap
// access in a sync.Mutex so it's safe to share across goroutines (the Bus
// runs each session in its own goroutine, and tests sometimes pass a
// single FakeProvider to a Runner shared by all sessions). Each Chat call
// is also allowed to sleep before returning, which is how
// TestBus_TwoSessions_Parallel measures wall-clock concurrency.

type FakeProvider struct {
	mu     sync.Mutex
	Script []LLMResponse
	calls  int
	Err    error // non-nil => Chat returns this error and never advances

	// Delay simulates a slow provider. Measured per Chat call.
	Delay time.Duration
}

func (f *FakeProvider) Chat(ctx context.Context, req ChatReq) (LLMResponse, error) {
	if f.Delay > 0 {
		// Sleep OUTSIDE the lock so two concurrent Chat calls don't
		// serialize on the lock — the whole point of the Bus is parallel
		// across sessions, and tests need that to be observable.
		select {
		case <-time.After(f.Delay):
		case <-ctx.Done():
			return LLMResponse{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return LLMResponse{}, f.Err
	}
	if f.calls >= len(f.Script) {
		return LLMResponse{}, errors.New("FakeProvider: script exhausted")
	}
	resp := f.Script[f.calls]
	f.calls++
	return resp, nil
}

// Calls returns how many times Chat has been invoked.
func (f *FakeProvider) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
