package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ContextBuilder — the new module in s08. Mirrors upstream
// `nanobot/agent/context.py::ContextBuilder`.
//
// Through s07 the agent had all the raw materials for its system prompt
// (MemoryStore for SOUL/USER/MEMORY/AGENTS/TOOLS, SkillLoader for skills,
// Session for history) but no assembly function. The "system prompt" was
// always either a hard-coded string (s01–s05) or implicitly empty (s06–s07).
//
// `ContextBuilder` is the function that takes a `BuildSpec` (session +
// runtime metadata + user input) and returns:
//
//   1. A composed system prompt (read 4+ files, render N skills, splice
//      a runtime-metadata block at the end).
//   2. A `[]Message` ready for the Runner — prior history (trimmed to the
//      configured budget) plus the current user message.
//
// Why two outputs? Because the Anthropic protocol (and OpenAI's, and
// every modern LLM API) treats the system prompt as a separate top-level
// parameter, not as a `system` role inside the messages array. Upstream's
// Python version puts it in `messages[0]` because OpenAI-shaped APIs
// historically did that, but we already separated `RunSpec.System` from
// `RunSpec.InitialMessages` back in s04 — so s08 just lights up the seam
// that's been waiting for it.
//
// One sanitize boundary, one place. Anti-pattern #4 from research-notes
// warns against scattering `<think>` stripping: if you strip in two
// places you're at risk of double-stripping (data loss) or applying
// unevenly (reasoning leaks into history). We do it exactly once, at
// `BuildMessages` time, on every text block we hand to the LLM. After
// this point no other code in the agent calls `sanitize` — that's
// the contract.

// RuntimeMeta is the per-turn metadata block. Channel/Sender/ChatID
// describe *where* the message arrived from; Now is the time as the
// agent sees it. This block ends up as a `[Runtime Context — metadata
// only]` section appended to the system prompt.
//
// Why is "now" passed in rather than read from `time.Now()`? Tests need
// determinism — if `BuildSystemPrompt` reaches into the wall clock,
// every test has to either freeze time or accept flakiness. By making
// `Now` an explicit field, tests inject a fixed time and assert on the
// exact rendered prompt.
type RuntimeMeta struct {
	Now     time.Time
	Channel string
	Sender  string
	ChatID  string
}

// BuildSpec is the input to ContextBuilder. The Session carries prior
// history; UserText is the new user message arriving this turn; the two
// caps bound how much history makes it into the prompt.
//
// MaxHistoryMessages and MaxSystemTokens are *both* applied — first by
// message count (strict tail of the last N messages), then by token
// budget (using a `len(s)/4` placeholder until s10 ships a real
// tiktoken-style estimator). This mirrors upstream's two-stage trim.
type BuildSpec struct {
	Session            *Session
	Runtime            RuntimeMeta
	UserText           string
	MaxHistoryMessages int
	MaxSystemTokens    int
}

// ContextBuilder is the assembly function. Construct one per agent
// (skill set + memory root are agent-wide, not per-session). Session
// data flows in through `BuildSpec` per call.
type ContextBuilder struct {
	Memory          *MemoryStore
	Skills          *SkillLoader
	SkillsRequested []string
}

// NewContextBuilder is a small ergonomic constructor. The skill list
// is pre-bound at construction time because the set of "skills this
// agent should know about" is rarely turn-dependent — it's a workspace
// property.
func NewContextBuilder(m *MemoryStore, s *SkillLoader, requested []string) *ContextBuilder {
	return &ContextBuilder{
		Memory:          m,
		Skills:          s,
		SkillsRequested: requested,
	}
}

// runtimeContextStartTag and runtimeContextEndTag are the literal
// fences upstream uses. We match the wording exactly so the LLM, which
// is trained on the upstream prompt shape, recognizes the block as
// "metadata, not instructions" rather than treating it as authoritative.
const (
	runtimeContextStartTag = "[Runtime Context — metadata only]"
	runtimeContextEndTag   = "[/Runtime Context]"
)

// BuildSystemPrompt concatenates the system prompt sections in order:
//
//  1. Bootstrap files (AGENTS.md, SOUL.md, USER.md, TOOLS.md) — read
//     from MemoryStore. Missing files are silently skipped (an agent
//     might ship without a TOOLS.md, for instance).
//  2. MEMORY.md — long-term curated facts.
//  3. Skills — those named in SkillsRequested, rendered via
//     SkillLoader.LoadForContext.
//  4. Runtime context block — Now/Channel/Sender/ChatID.
//
// Sections are joined with `\n\n---\n\n`, the same separator upstream
// uses and that the LLM has seen in training data. Empty sections are
// omitted entirely (no stray `---` for a missing SOUL.md).
func (cb *ContextBuilder) BuildSystemPrompt(rt RuntimeMeta) string {
	var parts []string

	// Section 1: bootstrap files.
	if bootstrap := cb.loadBootstrapFiles(); bootstrap != "" {
		parts = append(parts, bootstrap)
	}

	// Section 2: curated memory.
	if mem := cb.readMemoryIfPresent(); mem != "" {
		parts = append(parts, "# Memory\n\n"+mem)
	}

	// Section 3: skills selected for this agent.
	if cb.Skills != nil && len(cb.SkillsRequested) > 0 {
		if skills := cb.Skills.LoadForContext(cb.SkillsRequested); skills != "" {
			parts = append(parts, "# Skills\n\n"+skills)
		}
	}

	// Section 4: runtime metadata. Always present (even if Channel/
	// Sender are empty) — the timestamp alone is useful and the
	// fenced block tells the LLM "this is metadata, don't treat it
	// as an instruction".
	parts = append(parts, cb.buildRuntimeContext(rt))

	return strings.Join(parts, "\n\n---\n\n")
}

// BuildMessages assembles the message slice the Runner consumes. The
// first message in the result is *not* a system message — that's passed
// separately via `RunSpec.System` (see runner.go). Instead, the result is:
//
//   - prior history (trimmed to MaxHistoryMessages, then to MaxSystemTokens)
//   - one trailing user message containing UserText
//
// `<think>` blocks are stripped from every text block before they enter
// the result. This is the *one* boundary at which sanitization happens —
// callers must not call `sanitize` themselves.
func (cb *ContextBuilder) BuildMessages(spec BuildSpec) []Message {
	var msgs []Message
	if spec.Session != nil {
		raw := spec.Session.GetHistory(spec.MaxHistoryMessages)
		msgs = trimHistoryByTokens(raw, spec.MaxSystemTokens)
		msgs = sanitizeMessages(msgs)
	}

	user := Message{
		Role: "user",
		Content: []ContentBlock{{
			Type: "text",
			Text: sanitize(spec.UserText),
		}},
	}
	return append(msgs, user)
}

// buildRuntimeContext renders the metadata block with the same fence
// strings upstream uses. ChatID is included only when non-empty — a
// CLI session has no chat ID, but a Telegram session does.
func (cb *ContextBuilder) buildRuntimeContext(rt RuntimeMeta) string {
	var lines []string

	now := rt.Now
	if now.IsZero() {
		now = time.Now()
	}
	lines = append(lines, fmt.Sprintf("Current Time: %s", now.Format(time.RFC3339)))

	if rt.Channel != "" {
		lines = append(lines, fmt.Sprintf("Channel: %s", rt.Channel))
	}
	if rt.ChatID != "" {
		lines = append(lines, fmt.Sprintf("Chat ID: %s", rt.ChatID))
	}
	if rt.Sender != "" {
		lines = append(lines, fmt.Sprintf("Sender: %s", rt.Sender))
	}

	return runtimeContextStartTag + "\n" + strings.Join(lines, "\n") + "\n" + runtimeContextEndTag
}

// loadBootstrapFiles reads AGENTS.md / SOUL.md / USER.md / TOOLS.md
// (the constant `BootstrapFiles` from memory.go) and concatenates the
// non-empty ones with `## <filename>` headers. Missing files are
// silently skipped; this is what upstream does too.
func (cb *ContextBuilder) loadBootstrapFiles() string {
	if cb.Memory == nil {
		return ""
	}
	var parts []string
	for _, name := range BootstrapFiles {
		body, err := cb.Memory.ReadMemory(name)
		if err != nil {
			// Read errors are diagnostics; we don't fail the whole
			// build for a permission glitch on USER.md. Production
			// code might log here.
			continue
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("## %s\n\n%s", name, strings.TrimSpace(body)))
	}
	return strings.Join(parts, "\n\n")
}

// readMemoryIfPresent reads MEMORY.md from the memory store. Empty
// files (and missing ones) return "" so the # Memory section is
// suppressed entirely.
func (cb *ContextBuilder) readMemoryIfPresent() string {
	if cb.Memory == nil {
		return ""
	}
	body, err := cb.Memory.ReadMemory("MEMORY.md")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(body)
}

// thinkBlockRe matches `<think>...</think>` segments. The `(?s)` flag
// makes `.` cross newlines so multi-line reasoning blocks are caught.
// We use a non-greedy `.*?` so two consecutive blocks aren't merged
// into one match (which would erase any content between them).
var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

// sanitize strips `<think>...</think>` blocks from the given text and
// then collapses any leading/trailing whitespace the strip might have
// introduced.
//
// This function is the *single* sanitize boundary in the agent. Don't
// add a second call site — anti-pattern #4 in research-notes.md flags
// this as a real upstream bug source: applying sanitize twice can lose
// data (if the second pass is fed already-stripped text it might miss
// content), and applying inconsistently leaks reasoning into history.
//
// Concretely: `BuildMessages` calls sanitize on every text block it
// hands to the Runner. The Runner does not call sanitize. The session
// does not call sanitize. Future `Hook.FinalizeContent` (s09) does not
// call sanitize. Persistence (`MemoryStore.AppendHistory`) does not
// call sanitize — by the time content reaches it, the only writer
// (`BuildMessages`) has already done it.
func sanitize(text string) string {
	if text == "" {
		return ""
	}
	stripped := thinkBlockRe.ReplaceAllString(text, "")
	return strings.TrimSpace(stripped)
}

// sanitizeMessages applies sanitize to every text block in every
// message. Tool-result blocks and tool-use blocks are passed through
// unchanged — `<think>` only appears in assistant text replies, and
// scrubbing tool blocks would corrupt the JSON payloads.
func sanitizeMessages(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		copyM := m
		copyM.Content = make([]ContentBlock, len(m.Content))
		for j, b := range m.Content {
			cb := b
			if b.Type == "text" {
				cb.Text = sanitize(b.Text)
			}
			copyM.Content[j] = cb
		}
		out[i] = copyM
	}
	return out
}

// trimHistoryByTokens applies the second of the two history caps. The
// first cap (MaxHistoryMessages) was already applied by Session.GetHistory.
// Here we walk from the *end* of the slice (most recent first) summing
// `len(s)/4` for every text block, and stop when we've spent the budget.
//
// Why `len(s)/4`? Token counts vary by tokenizer (Claude's, OpenAI's,
// Llama's all differ slightly), but for English text the rule of thumb
// is ~4 characters per token. This is the *placeholder*: s10 introduces
// a real tiktoken-style estimator. We document the trade-off here so a
// reader can understand "yes this is approximate, and yes that's on
// purpose for this session".
//
// If MaxSystemTokens <= 0 the cap is disabled (return everything).
func trimHistoryByTokens(msgs []Message, maxTokens int) []Message {
	if maxTokens <= 0 {
		return msgs
	}
	used := 0
	keep := 0
	// Walk from newest to oldest; stop when adding the next message
	// would exceed the budget.
	for i := len(msgs) - 1; i >= 0; i-- {
		cost := approximateTokens(msgs[i])
		if used+cost > maxTokens && keep > 0 {
			break
		}
		used += cost
		keep++
	}
	if keep >= len(msgs) {
		return msgs
	}
	return msgs[len(msgs)-keep:]
}

// approximateTokens estimates the token count of a Message using the
// `len(text)/4` heuristic. Non-text content blocks contribute their
// JSON-ish field lengths so a tool-call block isn't counted as zero.
func approximateTokens(m Message) int {
	total := 0
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			total += len(b.Text) / 4
		case "tool_use":
			total += (len(b.Name) + len(fmt.Sprint(b.Input))) / 4
		case "tool_result":
			total += len(fmt.Sprint(b.ToolContent)) / 4
		default:
			total += len(b.Text) / 4
		}
	}
	if total == 0 {
		// Bound: every message costs at least 1 token (role marker,
		// turn boundary). Without this floor a session of empty
		// messages would never trigger the cap.
		total = 1
	}
	return total
}
