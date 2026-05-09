package main

import (
	"context"
	"errors"
)

// FakeProvider is the deterministic test double established in s03.
// Tests script a list of LLMResponses; FakeProvider returns them in order.
// No network, no API key. Reused unchanged here in s04.

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

// Calls returns how many times Chat has been invoked.
func (f *FakeProvider) Calls() int { return f.calls }
