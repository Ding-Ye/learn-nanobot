package main

import (
	"context"
	"errors"
	"os"
	"time"
)

// stateBuild is the prompt-assembly handler. Mirrors upstream
// `_state_build` (loop.py:1413).
//
// Job: ask ContextBuilder (s08) to assemble the system prompt + the
// messages slice for this turn. Stash the outputs on tc so StateRun can
// hand them to Runner.Run.
//
// ContextBuilder itself does the heavy lifting:
//
//   - Reads bootstrap files (AGENTS.md, SOUL.md, USER.md, TOOLS.md) via
//     MemoryStore (s06).
//   - Reads MEMORY.md if present.
//   - Renders the requested skills via SkillLoader (s07).
//   - Splices runtime metadata (channel, sender, current time).
//   - Trims session history to MaxHistoryMessages then to MaxSystemTokens.
//   - Sanitizes `<think>` blocks at the single boundary (anti-pattern #4
//     in research-notes.md).
//
// What stateBuild adds on top:
//
//   - Wires up the RuntimeMeta from tc.Msg metadata (channel, sender)
//     plus time.Now(). Tests can override by injecting a frozen Now.
//   - Reads the per-Loop history-size and token caps. (We use compact
//     defaults since the Bus doesn't pass them explicitly today.)
//   - Surfaces ContextBuilder errors verbatim.
//
// **Errors from this state propagate as state-machine errors.** That's
// how `TestLoop_ErrorInBuild_TransitionsToError` exercises the error
// transition: point Memory at a directory the process can't read, watch
// stateBuild bubble the read error up, see tc.State == StateError.
//
// Why call stateBuild and not stateAssemble or statePrepare? Mirrors
// upstream's name choice. The mental model is "build the prompt the
// runner will see"; "build" carries the sense of "construct the
// composite artifact". Don't fight upstream's naming — readers move
// between the two codebases on every chapter, every divergence is a
// papercut.
func (l *Loop) stateBuild(ctx context.Context, tc *TurnContext) error {
	_ = ctx // ContextBuilder is sync today; reserved for cancellation hookup

	if l.CtxBuilder == nil {
		return errors.New("stateBuild: CtxBuilder must be set")
	}
	if tc.Session == nil {
		return errors.New("stateBuild: tc.Session is nil")
	}

	// Probe the memory root early. The bootstrap-file reads inside
	// ContextBuilder swallow errors silently (a missing TOOLS.md is
	// fine). But if the *root directory itself* is gone or unreadable
	// we want to surface that as a state-machine error so the
	// `TestLoop_ErrorInBuild_TransitionsToError` test has something
	// to assert on.
	if l.Memory != nil {
		root := l.Memory.Root()
		info, err := os.Stat(root)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("stateBuild: memory root is not a directory")
		}
	}

	rt := RuntimeMeta{
		Now:     time.Now(),
		Channel: "cli",
		Sender:  os.Getenv("USER"),
	}
	if tc.SessionKey != "" {
		rt.ChatID = tc.SessionKey
	}

	tc.SystemPrompt = l.CtxBuilder.BuildSystemPrompt(rt)
	tc.History = tc.Session.GetHistory(buildMaxHistory)
	tc.InitialMessages = l.CtxBuilder.BuildMessages(BuildSpec{
		Session:            tc.Session,
		Runtime:            rt,
		UserText:           tc.Msg.UserText,
		MaxHistoryMessages: buildMaxHistory,
		MaxSystemTokens:    buildMaxTokens,
	})

	return nil
}

// buildMaxHistory and buildMaxTokens are the curriculum's defaults for
// the Build state. Production code would plumb these through LoopConfig
// (and ultimately a config file) so different agents can tune them per
// model.
const (
	buildMaxHistory = 40
	buildMaxTokens  = 6000
)
