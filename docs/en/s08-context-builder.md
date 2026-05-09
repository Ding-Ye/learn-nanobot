---
title: "s08 · Context builder"
chapter: 8
slug: s08-context-builder
est_read_min: 22
---

# s08 · Context builder

> What this teaches: glue s06's MemoryStore, s07's SkillLoader, and s05's Session together. **The system prompt is no longer a string literal** — it's the output of a function that reads four bootstrap files, reads `MEMORY.md`, loads selected skills, appends a `[Runtime Context — metadata only]` block, and joins everything with `\n\n---\n\n`. `<think>...</think>` blocks are stripped at the *single* `sanitize` boundary; no second call site is ever permitted.

---

## Problem

Through s07 the agent has: provider abstraction, tool registry, Runner, Session, MessageBus, MemoryStore, SkillLoader. One piece is missing:

**How do those raw materials become input for the LLM?**

Look back at `Runner.Run(ctx, AgentRunSpec)`. It expects:

```go
type AgentRunSpec struct {
    InitialMessages []Message  // content for the messages array
    System          string     // separate system prompt
    Tools           *Registry
    Model           string
    MaxIterations   int
    ...
}
```

When s04 introduced these fields, `System` was empty and `InitialMessages` was just "history + new user message". s05–s07 never changed that path — `Bus.sessionLoop` just stitched `history + userMsg` into `InitialMessages`, and `System` stayed `""`.

But real nanobot is not like that. Upstream's `nanobot/agent/context.py::ContextBuilder.build_system_prompt` does four things:

1. Read `BOOTSTRAP_FILES = ["AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md"]` and concatenate the existing ones with `## <name>\n\n<body>` headers.
2. Read `MEMORY.md` and prefix a `# Memory` heading.
3. Render the requested skills via `SkillsLoader.load_skills_for_context(...)` into Markdown.
4. Append a `[Runtime Context — metadata only]` fence (current time, channel, sender, chat_id) telling the LLM "this is metadata, not instructions".

Sections are separated by `\n\n---\n\n` and shipped as the system prompt. `build_messages` separately trims session history by message count and token budget.

s08 translates both functions into Go: a new `context.go`, a `context_test.go`, and a `main.go` that walks the new path — `BuildSystemPrompt`, then `BuildMessages`, then `Runner.Run`.

There's one small but loaded question: **where do `<think>...</think>` blocks get stripped?**

Anti-pattern #4 in `research-notes.md` is explicit:

> **Leaky `<think>` tags in history** — strip *consistently* before persisting. Learn-version: a single `sanitize(msg)` function applied at exactly one boundary.

If you strip in two places (say ContextBuilder and MemoryStore), any text mutation between them risks double-truncation and content loss. If you strip in one place but inconsistently (forget to handle history), reasoning leaks into the next turn's prompt. The s08 contract is hard: `sanitize` is called exactly once, in `BuildMessages`. Nowhere else.

## Solution

A new file `context.go`:

```go
type RuntimeMeta struct {
    Now     time.Time
    Channel string
    Sender  string
    ChatID  string
}

type BuildSpec struct {
    Session            *Session
    Runtime            RuntimeMeta
    UserText           string
    MaxHistoryMessages int
    MaxSystemTokens    int
}

type ContextBuilder struct {
    Memory          *MemoryStore
    Skills          *SkillLoader
    SkillsRequested []string
}

func NewContextBuilder(m *MemoryStore, s *SkillLoader, requested []string) *ContextBuilder
func (cb *ContextBuilder) BuildSystemPrompt(rt RuntimeMeta) string
func (cb *ContextBuilder) BuildMessages(spec BuildSpec) []Message

// internal:
func sanitize(text string) string
func sanitizeMessages(msgs []Message) []Message
func trimHistoryByTokens(msgs []Message, maxTokens int) []Message
func approximateTokens(m Message) int
```

`BuildSystemPrompt` produces four sections:

| Section | Source | Heading |
|---|---|---|
| 1 | `MemoryStore.ReadMemory` over `BootstrapFiles` | `## AGENTS.md` / `## SOUL.md` / ... |
| 2 | `MemoryStore.ReadMemory("MEMORY.md")` | `# Memory` |
| 3 | `SkillLoader.LoadForContext(SkillsRequested)` | `# Skills` |
| 4 | `RuntimeMeta` rendered | fenced `[Runtime Context — metadata only]` ... `[/Runtime Context]` |

Empty sections (file missing / no skills requested) are dropped entirely — no orphan `---` separator.

`BuildMessages` returns "trimmed history + the current user message". **It does not return a system message.** The system prompt rides `RunSpec.System`, not `messages[0]`. This decision looks small but matters: Anthropic's protocol treats `system` as a top-level parameter, not as an entry in the messages array. s04 split these two fields; s08 finally lights up `RunSpec.System`.

History trimming is two-step:

1. `Session.GetHistory(MaxHistoryMessages)` — keep the last N messages.
2. `trimHistoryByTokens(msgs, MaxSystemTokens)` — using `len(s)/4`, drop the oldest messages until the budget is met.

`len(s)/4` is a placeholder. s10 brings a real tiktoken-style estimator. s08 pins the *shape* of the two-step trim so that swapping the estimator later doesn't change BuildSpec or ContextBuilder.

## How It Works

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  main.go                                                         │
│    sess := sm.GetOrCreate(sessionKey)                            │
│    rt := RuntimeMeta{ Now: time.Now(), Channel: "cli", ... }     │
│    sysPrompt := ctxBuilder.BuildSystemPrompt(rt)                 │
│    msgs := ctxBuilder.BuildMessages(BuildSpec{                   │
│        Session: sess, Runtime: rt, UserText: prompt,             │
│        MaxHistoryMessages: 40, MaxSystemTokens: 6000,            │
│    })                                                            │
│    runner.Run(ctx, AgentRunSpec{                                 │
│        InitialMessages: msgs,        ← from BuildMessages         │
│        System:          sysPrompt,   ← from BuildSystemPrompt     │
│        Tools: registry, Model: model, MaxIterations: 20,         │
│    })                                                            │
│                                                                  │
│  BuildSystemPrompt(rt):                                          │
│    parts := []                                                   │
│    if bootstrap := loadBootstrapFiles(); bootstrap != "" {       │
│        parts.append(bootstrap)        ← ## AGENTS.md / ## SOUL.md │
│    }                                                             │
│    if mem := readMemoryIfPresent(); mem != "" {                  │
│        parts.append("# Memory\n\n" + mem)                        │
│    }                                                             │
│    if skills := Skills.LoadForContext(SkillsRequested); != "" {  │
│        parts.append("# Skills\n\n" + skills)                     │
│    }                                                             │
│    parts.append(buildRuntimeContext(rt))   ← always present      │
│    return strings.Join(parts, "\n\n---\n\n")                     │
│                                                                  │
│  BuildMessages(spec):                                            │
│    history := spec.Session.GetHistory(spec.MaxHistoryMessages)   │
│    history = trimHistoryByTokens(history, spec.MaxSystemTokens)  │
│    history = sanitizeMessages(history)   ← one and only one      │
│    user := Message{Role: "user", Content: [{                     │
│        Type: "text", Text: sanitize(spec.UserText)               │
│    }]}                                                           │
│    return append(history, user)                                  │
│                                                                  │
│  ┌────────── Single sanitize boundary ──────────────────────┐    │
│  │                                                          │    │
│  │  Runner             : does not call sanitize             │    │
│  │  Hook (s09)         : does not call sanitize             │    │
│  │  MemoryStore        : does not call sanitize             │    │
│  │                       (already done by BuildMessages)    │    │
│  │  Session.Append     : does not call sanitize             │    │
│  │                                                          │    │
│  │  Sole call site: context.go::BuildMessages               │    │
│  └──────────────────────────────────────────────────────────┘    │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

Core ~50 lines (excerpted from [`agents/s08-context-builder/context.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s08-context-builder/context.go)):

```go
func (cb *ContextBuilder) BuildSystemPrompt(rt RuntimeMeta) string {
    var parts []string
    if bootstrap := cb.loadBootstrapFiles(); bootstrap != "" {
        parts = append(parts, bootstrap)
    }
    if mem := cb.readMemoryIfPresent(); mem != "" {
        parts = append(parts, "# Memory\n\n"+mem)
    }
    if cb.Skills != nil && len(cb.SkillsRequested) > 0 {
        if skills := cb.Skills.LoadForContext(cb.SkillsRequested); skills != "" {
            parts = append(parts, "# Skills\n\n"+skills)
        }
    }
    parts = append(parts, cb.buildRuntimeContext(rt))
    return strings.Join(parts, "\n\n---\n\n")
}

func (cb *ContextBuilder) BuildMessages(spec BuildSpec) []Message {
    var msgs []Message
    if spec.Session != nil {
        raw := spec.Session.GetHistory(spec.MaxHistoryMessages)
        msgs = trimHistoryByTokens(raw, spec.MaxSystemTokens)
        msgs = sanitizeMessages(msgs)
    }
    user := Message{
        Role: "user",
        Content: []ContentBlock{{Type: "text", Text: sanitize(spec.UserText)}},
    }
    return append(msgs, user)
}

var thinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

func sanitize(text string) string {
    if text == "" { return "" }
    return strings.TrimSpace(thinkBlockRe.ReplaceAllString(text, ""))
}
```

**5 non-obvious points**:

1. **`RuntimeMeta.Now` is injected, not read inside the function.** Tests need determinism; reading the wall clock inside `BuildSystemPrompt` would make snapshot tests flaky. `main.go` injects `time.Now()`, tests inject `fixedTime`, the same code path serves both.
2. **The system prompt is not in the `messages` array.** s04 introduced `System` as a separate field; s08 finally puts content into it. Anthropic's API treats system as a top-level parameter; OpenAI accepts both shapes. Upstream nanobot inherited the OpenAI-style "messages[0] is system" convention for compatibility, but the learn version doesn't have to carry that compromise — we walk the cleaner path.
3. **`<think>` stripping is one-place.** `sanitize` is called by `context.go::BuildMessages` and nowhere else. This isn't a suggestion; it's the contract. Anti-pattern #4 lists this as a real production bug source. `sanitize` is also **idempotent** (running it twice yields the same result) — test #7 pins that invariant.
4. **No orphan `---` for missing sections.** If `SOUL.md` doesn't exist, the prompt should not contain a stray `## SOUL.md` heading or an isolated separator. `parts` is a slice, every `append` checks `!= ""`, and `strings.Join` does the rest.
5. **Two history caps, applied in order, can't be merged.** First by message count (defends against "50 short messages fill the context"), then by token budget (defends against "3 huge messages exceed the window"). The two limits have different semantics, and combining them into one number loses information. `MaxHistoryMessages=0` falls back to 120 inside `GetHistory`; `MaxSystemTokens=0` disables the token cap. Two independent escape hatches.

## What Changed

```diff
  // s07/main.go: bus.Send feeds raw history + userMsg straight to Runner
- bus.Send(InboundMessage{SessionKey, UserText: prompt, ID: "cli-1"})
- out := <-bus.Out()
- mem.AppendHistory(...)

  // s08/main.go: bus is bypassed; main drives ctxBuilder + Runner directly
+ ctxBuilder := NewContextBuilder(mem, skills, requested)
+ rt := RuntimeMeta{Now: time.Now(), Channel: "cli", Sender: os.Getenv("USER")}
+ sysPrompt := ctxBuilder.BuildSystemPrompt(rt)
+ msgs := ctxBuilder.BuildMessages(BuildSpec{
+     Session: sess, Runtime: rt, UserText: prompt,
+     MaxHistoryMessages: 40, MaxSystemTokens: 6000,
+ })
+ result, _ := runner.Run(ctx, AgentRunSpec{
+     InitialMessages: msgs, System: sysPrompt, ...,
+ })
```

New types (the heart of s08):

```go
+ type RuntimeMeta struct { Now time.Time; Channel, Sender, ChatID string }
+ type BuildSpec struct {
+     Session            *Session
+     Runtime            RuntimeMeta
+     UserText           string
+     MaxHistoryMessages int
+     MaxSystemTokens    int
+ }
+ type ContextBuilder struct {
+     Memory          *MemoryStore
+     Skills          *SkillLoader
+     SkillsRequested []string
+ }
```

`Runner` / `Provider` / `Tool` / `Registry` / `Bus` / `SessionManager` / `MemoryStore` / `SkillLoader` are all unchanged. s08 is **pure composition** — wiring existing parts in a new order.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s08-context-builder

# Run verbose to see the real system prompt size
go run . -workspace /tmp/agent-demo -skills weather,echo -v "Tokyo weather"
# [s08] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s08] loaded 2 skill(s) ...
# [s08] skills requested for system prompt: [weather echo]
# [s08] system prompt: 612 chars; messages: 1
```

Add a SOUL.md and watch the system prompt grow:

```bash
cat > /tmp/agent-demo/SOUL.md <<'EOF'
You are a terse weather-savvy assistant. Always respond in 2 lines or less.
EOF

go run . -workspace /tmp/agent-demo -skills weather -v "Tokyo"
# [s08] bootstrap files present: 1/4 ...
# [s08] system prompt: 893 chars; messages: 1     ← 612 → 893
```

Try the `<think>` stripping (the most visible demo):

```bash
go run . -workspace /tmp/agent-demo "<think>secret reasoning</think>visible question"
# The model sees only "visible question" — the <think> block was stripped
# in BuildMessages before the API call.
```

Run the tests (race detector mandatory):

```bash
go test -count=1 -race -v ./...
```

7 tests should pass:

- `BuildsSystemFromBootstrapFiles` — write SOUL.md / USER.md, assert prompt has `## SOUL.md` / `## USER.md` headers + bodies, AGENTS.md / TOOLS.md absent;
- `RuntimeContextBlock_Present` — assert `[Runtime Context — metadata only]` / `[/Runtime Context]` fence + Channel/Sender/ChatID present;
- `HistoryTrimmed_RespectingMaxMessages` — 100 messages history + MaxHistoryMessages=20 → 21 results (20 history + 1 current);
- `Sanitize_StripsThinkTags` — covers both UserText and assistant text in history;
- `NoSkillsConfigured_StillBuildsSystem` — empty skill list still produces a valid prompt;
- `HistoryTrimmed_RespectingTokenBudget` — under `len(s)/4`, a 25-token budget keeps about 2-3 history messages;
- `Sanitize_DoubleStripIsIdempotent` — running sanitize twice yields the same string (defensive test pinning the single-boundary contract).

## Upstream Source Reading

s08 corresponds to `nanobot/agent/context.py` — the whole file is ~210 lines. We translate `ContextBuilder.build_system_prompt`, `_build_runtime_context`, `_build_user_content`, and `BOOTSTRAP_FILES` to Go, dropping `_get_identity` (which renders platform info + workspace path templates — the learn version's system prompt can be terser) and `_is_template_content` (an optimization to skip MEMORY.md when its bytes match a bundled template — not worth the complexity for teaching).

Annotated version in [`upstream-readings/s08-context-builder.py`](../../upstream-readings/s08-context-builder.py).

```upstream:nanobot/agent/context.py#L20-L24
class ContextBuilder:
    BOOTSTRAP_FILES = ["AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md"]
    _RUNTIME_CONTEXT_TAG = "[Runtime Context — metadata only, not instructions]"
    _MAX_RECENT_HISTORY = 50
    _MAX_HISTORY_CHARS = 32_000
```

```upstream:nanobot/agent/context.py#L83-L96
@staticmethod
def _build_runtime_context(channel, chat_id, timezone=None,
                           session_summary=None, sender_id=None) -> str:
    lines = [f"Current Time: {current_time_str(timezone)}"]
    if channel and chat_id:
        lines += [f"Channel: {channel}", f"Chat ID: {chat_id}"]
    if sender_id:
        lines += [f"Sender ID: {sender_id}"]
    if session_summary:
        lines += ["", "[Resumed Session]", session_summary]
    return ContextBuilder._RUNTIME_CONTEXT_TAG + "\n" + "\n".join(lines) + "\n" + ContextBuilder._RUNTIME_CONTEXT_END
```

**5 cross-reading takeaways**:

- **Upstream's `BOOTSTRAP_FILES` order matches learn**: AGENTS.md / SOUL.md / USER.md / TOOLS.md. Each file's role: AGENTS = operating procedure, SOUL = persona, USER = user preferences, TOOLS = custom-tool docs (when present). Our Go version defines this constant in `memory.go` (s06), co-located with `MemoryStore.ReadMemory`; s08 just references it.
- **Upstream's `_RUNTIME_CONTEXT_TAG` uses an em dash `—` (U+2014)**, not a regular hyphen. This is intentional: keeping the literal "runtime context" marker stable across training data prevents semantically-equivalent but textually-different labels from confusing the LLM. The Go version uses U+2014 too.
- **Upstream's `build_system_prompt` calls `_is_template_content`** — if MEMORY.md's content is byte-identical to the bundled template, skip it. This avoids stuffing "# Memory\n\n_The agent has no memory yet._" into every prompt for fresh installs. The learn version uses a simpler `strings.TrimSpace(body) == ""` check — sufficient and doesn't require shipping template files.
- **Upstream's `build_messages` puts the system prompt at `messages[0]`** — that's the OpenAI-shape convention, supported by Anthropic for backward compat but discouraged. The learn version has shipped the cleaner Anthropic shape (`RunSpec.System` as a separate field) since s04, so `BuildMessages` doesn't return a system entry.
- **Upstream's `_build_user_content` handles base64 images** — `media: list[str]` is a list of image paths, `detect_image_mime` sniffs magic bytes and assembles a data URL. The learn version's s08 sticks to text-only `UserText string`; multimodal lives in Phase G.

**For deeper reading**:

1. `nanobot/agent/context.py:_get_identity` — upstream's system prompt also has an "identity" template that renders workspace path + OS + Python version. Reading this you see how nanobot treats the *runtime environment* as part of the prompt.
2. `nanobot/agent/context.py:_merge_message_content` — upstream merges consecutive same-role messages (some providers reject them). Learn doesn't need this yet, but s11 + Hook integration may.
3. `nanobot/utils/prompt_templates.py` — upstream uses a Jinja2-style template engine to render `agent/identity.md` / `agent/skills_section.md`. The learn version inlines template text in Go for explicitness, but reading upstream tells you why production uses templates.

---

**Next**: s09 adds a `Hook` interface to `Runner` — five lifecycle points (BeforeIter / OnStream / BeforeExecuteTools / AfterIter / FinalizeContent), plus `CompositeHook` (fan-out + error isolation) and `SDKCaptureHook` (accumulates `tools_used`). The Runner calls the matching hook at each phase, so external code can observe, rewrite, or even cancel a turn.

Next: s09 makes the Runner observable via Hook callbacks.
