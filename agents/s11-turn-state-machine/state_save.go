package main

import (
	"context"
	"errors"
)

// stateSave is the persistence handler. Mirrors upstream `_state_save`
// (loop.py:1475).
//
// Job:
//
//  1. Append the user message to MemoryStore (history.jsonl).
//  2. Append the assistant's final reply to MemoryStore.
//  3. Update Session.Messages so the next turn's StateBuild sees them.
//
// Why both the file-backed history and the in-memory session? Because
// they serve different consumers:
//
//   - **history.jsonl** is durable and append-only. It survives restarts.
//     The Dream cycle (App. A) reads it later to interpret what happened
//     across many sessions.
//
//   - **Session.Messages** is the per-process working memory. The next
//     turn's StateBuild slices it via Session.GetHistory to seed the
//     LLM. It would survive a single process lifetime; a restart loses
//     it (the next turn would have to re-load from history.jsonl, which
//     this curriculum doesn't implement — see the diligent-reader's
//     extension).
//
// **Anti-pattern dodged here**: research-notes.md anti-pattern #4 calls
// out the risk of "scattered sanitize calls". We don't sanitize in this
// file — ContextBuilder.BuildMessages already did it once at the
// boundary, and the Runner's Hook can do its own thing in
// FinalizeContent. By the time content reaches StateSave the data is
// already clean.
//
// Failures append-history-side are non-fatal *for the in-memory session
// update* — we still update Session.Messages so the next turn sees a
// consistent view. But we DO surface the persistence error so the
// driver loop transitions to StateError; the operator wants to know
// when disk persistence broke.
func (l *Loop) stateSave(ctx context.Context, tc *TurnContext) error {
	_ = ctx // memory writes are sync today

	if tc.Session == nil {
		return errors.New("stateSave: tc.Session is nil")
	}
	if tc.Result == nil {
		return errors.New("stateSave: tc.Result is nil (StateRun not run?)")
	}

	// 1) Persist the user message that drove this turn. Surface errors
	//    — operator wants to know when memory.jsonl breaks.
	if l.Memory != nil {
		if _, err := l.Memory.AppendHistory(HistoryEntry{
			Role:    "user",
			Content: tc.Msg.UserText,
		}); err != nil {
			return err
		}
		// 2) Persist the assistant reply with usage + tools_used metadata.
		if _, err := l.Memory.AppendHistory(HistoryEntry{
			Role:      "assistant",
			Content:   tc.Result.FinalContent,
			ToolsUsed: tc.Result.ToolsUsed,
			Usage:     tc.Result.Usage,
		}); err != nil {
			return err
		}
	}

	// 3) Update the in-memory session. The user message goes first
	//    (matches the order they entered the system); then any messages
	//    the Runner appended (assistant replies, tool_results) follow.
	userMsg := Message{
		Role:    "user",
		Content: []ContentBlock{{Type: "text", Text: tc.Msg.UserText}},
	}
	tc.Session.Append(userMsg)

	if len(tc.Result.Messages) > 0 {
		// Result.Messages includes everything the Runner saw (initial
		// messages + new ones it appended). We only want to persist the
		// new ones — slice past the original count.
		appendStart := len(tc.InitialMessages)
		if appendStart < len(tc.Result.Messages) {
			tc.Session.AppendAll(tc.Result.Messages[appendStart:])
		}
	}

	return nil
}
