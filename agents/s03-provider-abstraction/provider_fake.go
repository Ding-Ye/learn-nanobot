package main

import (
	"context"
	"errors"
)

// FakeProvider is a deterministic test double. Tests script a list of
// LLMResponses and FakeProvider returns them in order. No network, no API
// key. This is the test seam every later session reuses.

type FakeProvider struct {
	Script []LLMResponse
	calls  int
	Err    error // non-nil => Chat returns this error and never advances
}

func (f *FakeProvider) Chat(ctx context.Context, req ChatReq) (LLMResponse, error) {
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

// Calls returns how many times Chat has been invoked — useful in tests
// that want to assert "loop stopped after N turns".
func (f *FakeProvider) Calls() int { return f.calls }
