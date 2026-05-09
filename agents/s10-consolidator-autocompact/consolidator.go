package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Consolidator — the new memory-compression module in s10. Mirrors
// upstream `nanobot/agent/memory.py::Consolidator` (lines ~440–760 of
// memory.py).
//
// The job:
//
//  1. Find a *legal user-turn boundary* in `Session.Messages` that, if
//     dropped from the prompt, would free up `tokensToRemove` tokens.
//  2. Ask the LLM to summarize the dropped messages into a paragraph.
//  3. Persist the summary via `MemoryStore.WriteSummary` and the raw
//     interpretation event via `MemoryStore.AppendHistory`.
//  4. Advance `Session.LastConsolidated` so future history slices
//     don't re-include the dropped messages.
//
// Why a *legal user-turn* boundary? The Anthropic protocol (and OpenAI's
// in tool-use mode) requires the message stream to flow correctly: a
// `tool_use` block must be followed by a `tool_result` block with the
// matching `tool_use_id`; an `assistant` message cannot be the last
// thing the LLM sees if the model is supposed to keep talking. The
// safest cut-point is "right before a fresh `user` message that is NOT
// a tool_result" — because that's the natural turn boundary where a
// new conversation could legitimately start. Cutting anywhere else
// would orphan a tool_use or split an assistant turn.
//
// Token estimation is *approximate*. We ship a `len(s)/4` placeholder
// (the same one s08's ContextBuilder uses for history trimming) and
// expose it as a configurable function field so production callers can
// swap in a real tiktoken-style encoder. The trade-off is documented
// where it matters: in practice the placeholder is within ~2x of real
// token counts for English text; for code-heavy prompts it can be off
// by 3-4x. The boundary picker compensates by being conservative —
// it only stops when removed_tokens >= tokens_to_remove AND the next
// message is a clean user boundary.

// Boundary describes a half-open slice of messages selected for
// consolidation: messages[Start:End] will be summarized and dropped.
// Start matches the session's `LastConsolidated` cursor at the moment
// the boundary was picked; End is the index of the *next* user message
// (so messages[End] is preserved verbatim).
type Boundary struct {
	Start int
	End   int
}

// Consolidator picks boundaries, calls the LLM, and persists summaries.
// It does NOT own any goroutines — the AutoCompact module wraps it in
// a TTL ticker for background use; tests call Consolidate directly
// inline.
type Consolidator struct {
	// Provider is the LLM that summarizes the dropped messages. Tests
	// inject a FakeProvider; production wires up the same Anthropic
	// provider used elsewhere.
	Provider Provider

	// TokenEst is the per-string token estimator. Defaults to
	// `len(s)/4` (the same placeholder s08 uses); production callers
	// can swap in `pkoukk/tiktoken-go` or any other library.
	//
	// Why a function field? Because token estimation is *the* knob
	// you'd want to tune in production. Hard-coding a tiktoken
	// dependency would force every learn-nanobot reader to install
	// the upstream go-tiktoken bindings; making it configurable lets
	// the placeholder ship "good enough" for the curriculum and lets
	// production swap with one line.
	TokenEst func(string) int

	// ConsolidationRatio targets a fraction of the budget to retain
	// after consolidation. With ratio=0.5, we aim to remove enough
	// tokens to bring the prompt under half the context window.
	// Mirrors upstream's `consolidation_ratio = 0.5` default.
	ConsolidationRatio float64

	// Memory is the MemoryStore used to persist the summary to
	// `<root>/<sessionKey>-summary.md` and append a history event.
	Memory *MemoryStore

	// Model is the model id passed in the LLM request. Defaults to
	// "claude-sonnet-4-6" if zero — matches the rest of the
	// curriculum.
	Model string
}

// NewConsolidator constructs a Consolidator with sensible defaults: the
// `len(s)/4` token estimator and a 0.5 consolidation ratio. The Memory
// store is required (consolidation persists summaries to disk).
func NewConsolidator(p Provider, mem *MemoryStore) *Consolidator {
	return &Consolidator{
		Provider:           p,
		Memory:             mem,
		TokenEst:           defaultTokenEst,
		ConsolidationRatio: 0.5,
		Model:              "claude-sonnet-4-6",
	}
}

// defaultTokenEst is the `len(s)/4` placeholder. English text is ~4
// chars per token in BPE-style tokenizers; this works as a rough
// upper-bound for budget-tracking. Production swaps it for tiktoken.
func defaultTokenEst(s string) int {
	if len(s) < 4 {
		return 1
	}
	return len(s) / 4
}

// PickBoundary walks the session's messages from `LastConsolidated`
// forward, summing per-message token estimates, and returns the index
// pair (start, end) that satisfies BOTH:
//
//  1. The cumulative token estimate from start to end is at least
//     `tokensToRemove`.
//  2. messages[end] is a `user` message that is NOT a tool_result
//     replay — i.e. a clean turn boundary.
//
// If no such boundary exists (the session is too short, or every user
// message in range is a tool_result continuation), PickBoundary
// returns ok=false. Callers MUST check ok before using the boundary.
//
// Mirrors upstream's `pick_consolidation_boundary` in memory.py:880.
//
// The walk starts at `start` (= session.LastConsolidated) and only
// remembers the LAST candidate boundary that meets the token threshold.
// If multiple user messages all qualify, we return the latest (most
// recent) one — that maximizes how much we drop in this round and
// minimizes how often the consolidator has to re-run.
func (c *Consolidator) PickBoundary(s *Session, tokensToRemove int) (Boundary, bool) {
	if s == nil || tokensToRemove <= 0 {
		return Boundary{}, false
	}
	start := s.LastConsolidated
	if start < 0 {
		start = 0
	}
	if start >= len(s.Messages) {
		return Boundary{}, false
	}

	removed := 0
	var last Boundary
	found := false

	for idx := start; idx < len(s.Messages); idx++ {
		msg := s.Messages[idx]
		// A user message at idx > start is a candidate turn boundary
		// — *if* it's a fresh user prompt (not a tool_result reply).
		// Tool_result blocks have role="user" upstream, so we filter
		// them out by checking the content blocks.
		if idx > start && msg.Role == "user" && !isToolResultMessage(msg) && removed >= tokensToRemove {
			last = Boundary{Start: start, End: idx}
			found = true
		}
		removed += c.estimateMessageTokens(msg)
	}

	if !found {
		return Boundary{}, false
	}
	return last, true
}

// isToolResultMessage returns true if the message looks like a
// tool_result reply (role="user" but every content block is type
// "tool_result"). These are NOT legal cut points — they belong to the
// preceding assistant tool_use call and breaking them up would orphan
// the tool_use_id.
func isToolResultMessage(m Message) bool {
	if len(m.Content) == 0 {
		return false
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// estimateMessageTokens applies TokenEst to every text-bearing field
// in a message. Same shape as s08's `approximateTokens`, deliberately
// duplicated so the Consolidator owns its budget math.
func (c *Consolidator) estimateMessageTokens(m Message) int {
	est := c.TokenEst
	if est == nil {
		est = defaultTokenEst
	}
	total := 0
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			total += est(b.Text)
		case "tool_use":
			total += est(b.Name) + est(fmt.Sprint(b.Input))
		case "tool_result":
			total += est(fmt.Sprint(b.ToolContent))
		default:
			total += est(b.Text)
		}
	}
	if total == 0 {
		// Floor: every message costs at least 1 token (role, turn
		// boundary). Without this floor a session of empty messages
		// would never trigger the cap.
		total = 1
	}
	return total
}

// summarizationSystem is the system prompt for the summarizer. Phrased
// to match upstream's intent: preserve facts + tool outcomes + user
// preferences; output a paragraph (not bullet points), suitable for
// re-injection into a future system prompt.
const summarizationSystem = `You are a memory consolidator for an AI agent.

You will receive a chunk of conversation history (messages between a user
and an assistant, possibly including tool calls and tool results). Your
job is to write a tight, factual summary that future-you will load back
into context when this conversation resumes.

Keep:
  - User preferences ("the user prefers terse answers"), goals, and
    explicit constraints.
  - Factual outcomes of tool calls (what was found, what was created),
    not the raw tool output.
  - Decisions, blockers, and open questions.

Drop:
  - Verbose tool output, scratchpad reasoning, repeated greetings.
  - Anything the user already corrected.

Output: a single paragraph (no bullet points, no markdown headers).
Aim for 2-6 sentences. If there's nothing meaningful to summarize, output
exactly the string "(nothing)".`

// Consolidate is the high-level entry point: pick a boundary, build a
// summarization prompt, call Provider.Chat, persist the summary, and
// advance the cursor.
//
// On success, returns the summary string. On failure to find a legal
// boundary, returns ("", ErrNoLegalBoundary). On provider/persist
// errors, returns the wrapped error.
//
// Sequence (mirrors upstream `Consolidator.archive` + `consolidate`):
//
//  1. Estimate the un-consolidated message budget; compute
//     `tokensToRemove` as half the current excess (or just enough to
//     hit `ConsolidationRatio` of the budget).
//  2. PickBoundary; if false, abort with ErrNoLegalBoundary.
//  3. Render messages[Start:End] into a single user-message string
//     for the LLM (one line per message: "[role] content").
//  4. Call Provider.Chat with summarizationSystem + the rendered
//     messages.
//  5. WriteSummary(sessionKey, summary) and AppendHistory({role:
//     "system", content: "[summary] " + summary}).
//  6. Advance s.LastConsolidated to boundary.End.
//
// The Memory writes are best-effort sequenced: WriteSummary first
// (the canonical curated artifact), then AppendHistory (the audit
// breadcrumb). If the AppendHistory write fails we still advance
// LastConsolidated — the summary file is the source of truth.
func (c *Consolidator) Consolidate(ctx context.Context, s *Session) (string, error) {
	if s == nil {
		return "", errors.New("Consolidator.Consolidate: session must be non-nil")
	}
	if c.Provider == nil {
		return "", errors.New("Consolidator.Consolidate: provider must be set")
	}
	if c.Memory == nil {
		return "", errors.New("Consolidator.Consolidate: memory store must be set")
	}

	// Compute tokensToRemove from the un-consolidated tail's total
	// estimate. Default ratio is 0.5 — we aim to drop the older half
	// of the un-consolidated tail.
	ratio := c.ConsolidationRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.5
	}
	tail := 0
	for i := s.LastConsolidated; i < len(s.Messages); i++ {
		tail += c.estimateMessageTokens(s.Messages[i])
	}
	tokensToRemove := int(float64(tail) * ratio)
	if tokensToRemove < 1 {
		tokensToRemove = 1
	}

	boundary, ok := c.PickBoundary(s, tokensToRemove)
	if !ok {
		return "", ErrNoLegalBoundary
	}

	// Render the selected slice. Each message becomes one line:
	//   [role] text/tool_use/tool_result content
	// Tool blocks are rendered as their body so the LLM can see what
	// happened; we don't reconstruct the raw JSON.
	var rendered strings.Builder
	for i := boundary.Start; i < boundary.End; i++ {
		rendered.WriteString(formatMessageForSummary(s.Messages[i]))
		rendered.WriteString("\n")
	}

	resp, err := c.Provider.Chat(ctx, ChatReq{
		System: summarizationSystem,
		Messages: []Message{
			{Role: "user", Content: []ContentBlock{{Type: "text", Text: rendered.String()}}},
		},
		Model: c.Model,
	})
	if err != nil {
		return "", fmt.Errorf("Consolidator.Consolidate: provider call: %w", err)
	}
	if resp.FinishReason == "error" {
		return "", fmt.Errorf("Consolidator.Consolidate: provider returned error %s: %s",
			resp.ErrCode, resp.ErrMessage)
	}

	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		summary = "(no summary)"
	}

	// Persist: summary file first, then audit breadcrumb.
	if err := c.Memory.WriteSummary(s.Key, summary); err != nil {
		return "", fmt.Errorf("Consolidator.Consolidate: write summary: %w", err)
	}
	if _, err := c.Memory.AppendHistory(HistoryEntry{
		Role:    "system",
		Content: "[consolidator] " + summary,
	}); err != nil {
		// AppendHistory failure is logged but doesn't roll back the
		// summary file — the summary is the canonical artifact.
		// Production code might log; tests don't care.
		_ = err
	}

	// Advance the cursor. After this point the un-consolidated tail
	// shrinks to `messages[boundary.End:]`.
	s.LastConsolidated = boundary.End

	return summary, nil
}

// ErrNoLegalBoundary is returned by PickBoundary / Consolidate when no
// legal user-turn boundary exists in the session for the requested
// token-removal target. Tests check this with errors.Is.
var ErrNoLegalBoundary = errors.New("Consolidator: no legal user-turn boundary found")

// formatMessageForSummary renders a Message into a single line for the
// summarization prompt. Each role gets a tag; tool blocks are flattened
// into their bodies. We don't quote-escape anything because the LLM is
// reading prose, not structured input.
func formatMessageForSummary(m Message) string {
	var body strings.Builder
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			body.WriteString(b.Text)
			body.WriteString(" ")
		case "tool_use":
			fmt.Fprintf(&body, "[tool_use:%s %v] ", b.Name, b.Input)
		case "tool_result":
			fmt.Fprintf(&body, "[tool_result:%v] ", b.ToolContent)
		}
	}
	return fmt.Sprintf("[%s] %s", strings.ToUpper(m.Role), strings.TrimSpace(body.String()))
}
