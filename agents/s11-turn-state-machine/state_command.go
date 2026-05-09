package main

import (
	"context"
	"strings"
)

// stateCommand checks the inbound user text for slash commands. Mirrors
// upstream `_state_command` (loop.py:1402) at a very simplified level.
//
// Upstream's version dispatches through a `CommandRouter` (`/dream`,
// `/help`, `/clear`, `/onboard`, etc.); we ship the two most teaching-
// relevant commands inline:
//
//   - `/help` — emit a canned help message; do NOT call the LLM.
//   - `/clear` — reset Session.Messages; emit a "cleared" acknowledgement.
//
// Anything else passes through. The pattern is the interesting part —
// **slash commands short-circuit the state machine.** When a command
// matches, this handler:
//
//  1. Writes the canned reply into tc.FinalContent.
//  2. Sets tc.State = StateRespond directly.
//
// The driver loop sees that tc.State changed during dispatch and respects
// it (rather than consulting Transitions[StateCommand], which would send
// us to StateBuild). That's how we skip the entire BUILD/RUN/SAVE chain
// for `/help`: the LLM is never called, no history is read, no tools are
// invoked. RESPOND just relays the canned text and DONE follows.
//
// Why have a Command state at all if it's mostly a pass-through? Because
// it's a clean place to put the kind of "did the user want a side-effect
// rather than an LLM round-trip?" check. Putting it before BUILD means
// `/clear` doesn't waste tokens reading the about-to-be-cleared history;
// putting it after COMPACT means `/help` sees the same up-to-date session
// state every other state sees.
const helpText = `Available commands:
  /help    Show this help text.
  /clear   Reset the current session's message history.

Anything else is treated as a normal user message and dispatched to the LLM.`

func (l *Loop) stateCommand(ctx context.Context, tc *TurnContext) error {
	_ = ctx // commands are sync today

	raw := strings.TrimSpace(tc.Msg.UserText)
	if !strings.HasPrefix(raw, "/") {
		// Not a command — pass through to BUILD via the Transitions
		// table. Don't touch tc.State; the driver advances naturally.
		return nil
	}

	// Strip leading slash and split into word + rest. We only care about
	// the leading word for routing today.
	cmd := strings.SplitN(raw[1:], " ", 2)[0]
	cmd = strings.ToLower(cmd)

	switch cmd {
	case "help":
		tc.FinalContent = helpText
		// Skip BUILD/RUN/SAVE — jump straight to RESPOND. RESPOND will
		// see tc.FinalContent already populated and pass it through.
		tc.State = StateRespond
		return nil

	case "clear":
		if tc.Session != nil {
			tc.Session.Messages = nil
			tc.Session.LastConsolidated = 0
		}
		tc.FinalContent = "Session cleared. (next message starts a fresh turn)"
		tc.State = StateRespond
		return nil

	default:
		// Unknown command — let it pass through to the LLM. The model
		// will probably tell the user "I don't recognize /foo" which is
		// fine for the curriculum's purposes. Production code might
		// instead emit an "unknown command" canned reply here.
		return nil
	}
}
