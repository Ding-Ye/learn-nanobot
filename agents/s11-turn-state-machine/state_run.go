package main

import (
	"context"
	"errors"
)

// stateRun is the LLM ↔ tools tight loop. Mirrors upstream `_state_run`
// (loop.py:1452).
//
// Job: hand the messages StateBuild assembled to Runner.Run, capture the
// AgentRunResult on tc, and return. From the *outer* state machine's
// perspective this is one transition — the inner loop iterates as many
// times as the model needs (provider call → tool call → provider call → ...
// → finish_reason="stop").
//
// Two loops, two responsibilities — see turnstate.go's docstring for
// the rationale. The split is what lets us teach the inner loop in s04
// (where the surrounding state machine doesn't exist yet) and graft on
// the outer loop here in s11 without changing s04 at all. That's the
// entire compositional argument for the chapter.
//
// stateRun does NOT:
//
//   - Save messages to memory. That's StateSave's job.
//   - Format a final reply. That's StateRespond's job.
//   - Re-acquire the session lock. The Bus already owns it.
//   - Call the Hook directly. The Runner threads the Hook through every
//     iteration internally.
//
// It does exactly one thing: invoke Runner.Run. That single
// responsibility is what keeps the state machine readable.
func (l *Loop) stateRun(ctx context.Context, tc *TurnContext) error {
	if l.Runner == nil {
		return errors.New("stateRun: Runner must be set")
	}
	if len(tc.InitialMessages) == 0 {
		return errors.New("stateRun: tc.InitialMessages is empty (StateBuild not run?)")
	}

	spec := AgentRunSpec{
		InitialMessages: tc.InitialMessages,
		Tools:           l.Tools,
		Model:           l.Model,
		MaxIterations:   l.MaxIter,
		System:          tc.SystemPrompt,
		Verbose:         l.Verbose,
	}

	result, err := l.Runner.Run(ctx, spec)
	if err != nil {
		return err
	}
	tc.Result = &result
	tc.ToolsUsed = append([]string(nil), result.ToolsUsed...)
	return nil
}
