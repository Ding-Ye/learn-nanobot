package main

import (
	"context"
)

// stateRespond is the final-content assembly handler. Mirrors upstream
// `_state_respond` (loop.py:1501).
//
// Job: set tc.FinalContent to the user-facing text. The Bus driver in
// main.go reads tc.FinalContent after Process() returns and forwards it
// down whatever channel originated the inbound (CLI stdout, Telegram
// webhook, etc.).
//
// Two cases:
//
//  1. **The state machine walked normally** through RUN → SAVE →
//     RESPOND. tc.FinalContent is empty so far; we fill it from
//     tc.Result.FinalContent.
//
//  2. **A slash command short-circuited** to RESPOND from COMMAND.
//     tc.FinalContent was already set by stateCommand to the canned
//     reply ("/help" text or "Session cleared." message). We must
//     NOT overwrite it.
//
// Why a dedicated state for what looks like a one-line assignment?
// Because in upstream the RESPOND state does substantially more —
// formats the OutboundMessage, attaches generated media paths,
// dispatches to the channel via MessageBus.send, runs the
// `finalize_content` hook chain. We collapse that to "set FinalContent"
// because the Bus driver in main.go does the rest, but the *seam* is
// the teaching point: this is the boundary where "what the agent
// computed" turns into "what the user sees". Future work that adds
// streaming, multipart messages, or formatted attachments slots in
// here.
func (l *Loop) stateRespond(ctx context.Context, tc *TurnContext) error {
	_ = ctx // sync

	// Case 2: stateCommand already populated FinalContent. Respect that.
	if tc.FinalContent != "" {
		return nil
	}

	// Case 1: pull from the Runner's result.
	if tc.Result != nil {
		tc.FinalContent = tc.Result.FinalContent
	}

	// If we got here with no Result and no pre-populated FinalContent,
	// emit a neutral placeholder rather than crashing the channel. This
	// matches upstream's `EMPTY_FINAL_RESPONSE_MESSAGE` pattern.
	if tc.FinalContent == "" {
		tc.FinalContent = "(no response)"
	}

	return nil
}
