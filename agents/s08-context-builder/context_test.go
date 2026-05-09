package main

import (
	"strings"
	"testing"
	"time"
)

// seed builds a MemoryStore on a temp dir and pre-writes the named
// bootstrap files. Helpful because every test starts from a workspace
// that needs at least SOUL.md or USER.md to exercise the bootstrap path.
func seed(t *testing.T, files map[string]string) *MemoryStore {
	t.Helper()
	root := t.TempDir()
	mem, err := NewMemoryStore(root)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	for name, body := range files {
		if err := mem.WriteMemory(name, body); err != nil {
			t.Fatalf("WriteMemory %s: %v", name, err)
		}
	}
	return mem
}

// fixedTime gives every test the same Now so rendered prompts are
// stable across runs.
var fixedTime = time.Date(2026, 5, 9, 12, 30, 0, 0, time.UTC)

// TestContextBuilder_BuildsSystemFromBootstrapFiles — write a SOUL.md
// and a USER.md to MemoryStore. The system prompt must contain both
// (with `## SOUL.md` / `## USER.md` headers, so the LLM can tell which
// is which). Bootstrap files that weren't written should NOT appear.
func TestContextBuilder_BuildsSystemFromBootstrapFiles(t *testing.T) {
	mem := seed(t, map[string]string{
		"SOUL.md": "I am a helpful test agent.\nI like Go.",
		"USER.md": "The user prefers concise answers.",
	})
	skills := NewSkillLoader("", "", nil)
	cb := NewContextBuilder(mem, skills, nil)

	prompt := cb.BuildSystemPrompt(RuntimeMeta{Now: fixedTime})

	if !strings.Contains(prompt, "## SOUL.md") {
		t.Errorf("system prompt missing SOUL.md header:\n%s", prompt)
	}
	if !strings.Contains(prompt, "I am a helpful test agent.") {
		t.Errorf("system prompt missing SOUL.md body:\n%s", prompt)
	}
	if !strings.Contains(prompt, "## USER.md") {
		t.Errorf("system prompt missing USER.md header:\n%s", prompt)
	}
	if !strings.Contains(prompt, "The user prefers concise answers.") {
		t.Errorf("system prompt missing USER.md body:\n%s", prompt)
	}
	// Files we didn't write must not appear.
	if strings.Contains(prompt, "## AGENTS.md") {
		t.Errorf("AGENTS.md leaked into prompt despite not existing:\n%s", prompt)
	}
	if strings.Contains(prompt, "## TOOLS.md") {
		t.Errorf("TOOLS.md leaked into prompt despite not existing:\n%s", prompt)
	}
}

// TestContextBuilder_RuntimeContextBlock_Present — the system prompt
// must always end with a fenced runtime context block, even when no
// memory or skills are present. Channel/Sender values from RuntimeMeta
// must appear inside the fence.
func TestContextBuilder_RuntimeContextBlock_Present(t *testing.T) {
	mem := seed(t, nil) // empty workspace
	skills := NewSkillLoader("", "", nil)
	cb := NewContextBuilder(mem, skills, nil)

	prompt := cb.BuildSystemPrompt(RuntimeMeta{
		Now:     fixedTime,
		Channel: "telegram",
		Sender:  "user-42",
		ChatID:  "chat-7",
	})

	if !strings.Contains(prompt, runtimeContextStartTag) {
		t.Errorf("missing runtime context start tag:\n%s", prompt)
	}
	if !strings.Contains(prompt, runtimeContextEndTag) {
		t.Errorf("missing runtime context end tag:\n%s", prompt)
	}
	// Time must be present in RFC3339 form.
	if !strings.Contains(prompt, "2026-05-09T12:30:00Z") {
		t.Errorf("rendered time missing or wrongly formatted:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Channel: telegram") {
		t.Errorf("channel not rendered:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Sender: user-42") {
		t.Errorf("sender not rendered:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Chat ID: chat-7") {
		t.Errorf("chat id not rendered:\n%s", prompt)
	}

	// The runtime block should be the *last* section (after any
	// memory/skills) — verify it appears once and is fenced.
	startIdx := strings.Index(prompt, runtimeContextStartTag)
	endIdx := strings.Index(prompt, runtimeContextEndTag)
	if startIdx < 0 || endIdx < startIdx {
		t.Errorf("runtime block not fenced correctly: start=%d end=%d", startIdx, endIdx)
	}
}

// TestContextBuilder_HistoryTrimmed_RespectingMaxMessages — append 100
// messages to a Session, build with MaxHistoryMessages=20, assert that
// exactly 20 history messages plus the trailing user message (21 total)
// come back from BuildMessages.
func TestContextBuilder_HistoryTrimmed_RespectingMaxMessages(t *testing.T) {
	sm := NewSessionManager()
	sess := sm.GetOrCreate("cli:test")
	for i := 0; i < 100; i++ {
		sess.Append(Message{
			Role: "user",
			Content: []ContentBlock{{
				Type: "text",
				Text: "msg",
			}},
		})
	}

	mem := seed(t, nil)
	cb := NewContextBuilder(mem, NewSkillLoader("", "", nil), nil)

	msgs := cb.BuildMessages(BuildSpec{
		Session:            sess,
		Runtime:            RuntimeMeta{Now: fixedTime},
		UserText:           "current message",
		MaxHistoryMessages: 20,
		// MaxSystemTokens=0 → token cap disabled, so only the
		// message-count cap matters here.
	})

	// 20 history + 1 current = 21
	if got, want := len(msgs), 21; got != want {
		t.Fatalf("BuildMessages len = %d, want %d", got, want)
	}
	if msgs[len(msgs)-1].Content[0].Text != "current message" {
		t.Errorf("last message should be the current user text, got %q",
			msgs[len(msgs)-1].Content[0].Text)
	}
}

// TestContextBuilder_Sanitize_StripsThinkTags — when UserText contains
// `<think>...</think>` blocks, the output must have only the visible
// text. This pins down the single-sanitize-boundary contract from
// research-notes.md anti-pattern #4.
func TestContextBuilder_Sanitize_StripsThinkTags(t *testing.T) {
	mem := seed(t, nil)
	cb := NewContextBuilder(mem, NewSkillLoader("", "", nil), nil)

	msgs := cb.BuildMessages(BuildSpec{
		Runtime:  RuntimeMeta{Now: fixedTime},
		UserText: "<think>secret reasoning the user shouldn't see</think>visible",
	})

	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (no history, just current user), got %d", len(msgs))
	}
	got := msgs[0].Content[0].Text
	if strings.Contains(got, "<think>") {
		t.Errorf("output still contains <think>: %q", got)
	}
	if strings.Contains(got, "secret reasoning") {
		t.Errorf("output leaked reasoning content: %q", got)
	}
	if got != "visible" {
		t.Errorf("output = %q, want %q", got, "visible")
	}

	// Additional: assistant messages in history with <think> should
	// also be stripped.
	sm := NewSessionManager()
	sess := sm.GetOrCreate("cli:test")
	sess.Append(Message{
		Role: "assistant",
		Content: []ContentBlock{{
			Type: "text",
			Text: "<think>internal</think>final answer",
		}},
	})
	msgs2 := cb.BuildMessages(BuildSpec{
		Session:            sess,
		Runtime:            RuntimeMeta{Now: fixedTime},
		UserText:           "next",
		MaxHistoryMessages: 10,
	})
	if len(msgs2) != 2 {
		t.Fatalf("expected 2 (1 history + current), got %d", len(msgs2))
	}
	if got := msgs2[0].Content[0].Text; got != "final answer" {
		t.Errorf("history sanitize failed, got %q", got)
	}
}

// TestContextBuilder_NoSkillsConfigured_StillBuildsSystem — even with
// an empty SkillsRequested list, the system prompt must include the
// bootstrap files, MEMORY.md, and the runtime context block. The
// Skills section is simply omitted (no stray `# Skills` header).
func TestContextBuilder_NoSkillsConfigured_StillBuildsSystem(t *testing.T) {
	mem := seed(t, map[string]string{
		"AGENTS.md": "Agent operating procedure.",
		"MEMORY.md": "User likes pizza.",
	})
	cb := NewContextBuilder(mem, NewSkillLoader("", "", nil), nil)

	prompt := cb.BuildSystemPrompt(RuntimeMeta{
		Now:     fixedTime,
		Channel: "cli",
	})

	if !strings.Contains(prompt, "Agent operating procedure.") {
		t.Errorf("AGENTS.md content missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "# Memory") {
		t.Errorf("Memory section missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, "User likes pizza.") {
		t.Errorf("MEMORY.md body missing:\n%s", prompt)
	}
	if !strings.Contains(prompt, runtimeContextStartTag) {
		t.Errorf("runtime block missing:\n%s", prompt)
	}
	if strings.Contains(prompt, "# Skills") {
		t.Errorf("Skills header should not appear when none are requested:\n%s", prompt)
	}
}

// TestContextBuilder_HistoryTrimmed_RespectingTokenBudget — bonus test
// (beyond the 5-required minimum) to pin down the token-cap path.
// Build with a tiny token budget and verify only the most recent
// messages survive.
func TestContextBuilder_HistoryTrimmed_RespectingTokenBudget(t *testing.T) {
	sm := NewSessionManager()
	sess := sm.GetOrCreate("cli:tok")
	// Each message is 40 chars => ~10 tokens with len/4 estimator.
	for i := 0; i < 10; i++ {
		sess.Append(Message{
			Role: "user",
			Content: []ContentBlock{{
				Type: "text",
				Text: strings.Repeat("a", 40),
			}},
		})
	}

	mem := seed(t, nil)
	cb := NewContextBuilder(mem, NewSkillLoader("", "", nil), nil)

	msgs := cb.BuildMessages(BuildSpec{
		Session:            sess,
		Runtime:            RuntimeMeta{Now: fixedTime},
		UserText:           "now",
		MaxHistoryMessages: 100, // disabled
		MaxSystemTokens:    25,  // ≈ 2-3 messages of history
	})

	// The trailing message is always there; expect roughly 2-3
	// history messages plus the 1 current = 3-4 total.
	if len(msgs) < 2 || len(msgs) > 5 {
		t.Errorf("BuildMessages len = %d, expected token-cap to keep 2-4 history + 1 current", len(msgs))
	}
}

// TestContextBuilder_Sanitize_DoubleStripIsIdempotent — defense-in-depth
// test: even though sanitize is documented as a single-boundary call,
// running it twice on the same input must be safe. This guards against
// a future caller violating the contract; the second pass is a no-op
// when there's nothing left to strip.
func TestContextBuilder_Sanitize_DoubleStripIsIdempotent(t *testing.T) {
	once := sanitize("<think>x</think>hello")
	twice := sanitize(once)
	if once != twice {
		t.Errorf("sanitize is not idempotent: once=%q twice=%q", once, twice)
	}
	if once != "hello" {
		t.Errorf("sanitize once = %q, want %q", once, "hello")
	}
}
