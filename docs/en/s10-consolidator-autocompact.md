---
title: "s10 · Consolidator + AutoCompact"
chapter: 10
slug: s10-consolidator-autocompact
est_read_min: 28
---

# s10 · Consolidator + AutoCompact

> What this teaches: file-backed history grows without bound. After many turns the prompt blows the model's context window, latency climbs, cost climbs, and eventually the request just fails. Upstream's answer is a two-piece pipeline: `Consolidator` picks a *legal user-turn boundary* in `Session.Messages`, asks the LLM to summarize the dropped slice into a paragraph, persists the summary to `<sessionKey>-summary.md`, and advances `Session.LastConsolidated` past the boundary. `AutoCompact` is the TTL ticker that runs the consolidator on idle sessions in the background, so the next time the user comes back the prompt is already small. The chapter has three teaching points: (1) you can't summarize at any cut-point — protocol legality matters; (2) token estimation is approximate and that's on purpose; (3) AutoCompact is fire-and-forget and idempotent so it never races the agent loop.

---

## Problem

Through s09 the agent loop is feature-complete in shape: provider abstraction, tool registry, runner, sessions, bus, memory, skills, context, hooks. But it has one operational landmine: every turn appends to `Session.Messages`, and every system-prompt assembly reads the full un-consolidated tail. Run a CLI session for 50 turns of "summarize this PDF" and "search for that fact" and you'll watch the system prompt slide from 600 chars to 60 KB. Token counts climb past the budget, the model returns `finish_reason="length"` more often, the length-recovery path kicks in, and eventually the next request 400s with "request exceeds context window."

You can't fix this by trimming the tail dumbly. The Anthropic protocol — and OpenAI's, in tool-use mode — has structural rules about message order. A `tool_use` block must be followed by a `tool_result` block with the matching `tool_use_id`; an `assistant` message cannot be the *last* turn the LLM sees if it's expected to keep talking; a `user` message that's actually a tool_result reply belongs to its preceding tool_use call and breaking them up orphans the call. So you need **a cut-point that's structurally legal**: somewhere the message stream could plausibly be paused and resumed without the LLM noticing.

Upstream's answer is `Consolidator.pick_consolidation_boundary` in `nanobot/agent/memory.py:880`. It walks the un-consolidated tail starting at `Session.LastConsolidated`, sums per-message token estimates, and returns the latest user-message index where (a) cumulative removed tokens meet the target threshold and (b) the message is a fresh user prompt (not a tool_result reply). If no such index exists, it returns None and the caller falls back to a "raw archive" mode that just dumps the messages without summarization (we don't ship this fallback in s10 — we surface it as `ErrNoLegalBoundary` and let the caller decide).

Once you have the boundary, the rest is mechanical: format `messages[start:end]` as text, call `Provider.Chat` with a summarization system prompt, write the result to `<sessionKey>-summary.md`, advance `Session.LastConsolidated` to `end`. Future turns slice from `LastConsolidated` forward, so the summarized chunk vanishes from the prompt. The summary itself can be re-injected into the next system prompt (App. A's "memory as interpretation" pattern) or kept on disk as an audit artifact.

The wrinkle nobody warns you about: doing this synchronously *during* a turn adds an extra LLM round-trip to a turn that's already over budget. So upstream offloads it to a background ticker — `AutoCompact` — that runs on idle sessions. When the user comes back the prompt is already half its old size, and the foreground turn pays no consolidation cost. The trade-off is that the summarizer can race the bus on `Session.Messages` if the bus picks up the same session mid-consolidation, so AutoCompact has to (1) skip sessions that are currently active and (2) skip sessions that are already being consolidated. Both invariants are enforced by an `archiving` map keyed on session key.

## Solution

Three new types in `consolidator.go`:

```go
type Boundary struct { Start, End int }

type Consolidator struct {
    Provider           Provider
    TokenEst           func(string) int
    ConsolidationRatio float64
    Memory             *MemoryStore
    Model              string
}

var ErrNoLegalBoundary = errors.New("Consolidator: no legal user-turn boundary found")
```

Plus the workhorse methods:

```go
func NewConsolidator(p Provider, mem *MemoryStore) *Consolidator
func (c *Consolidator) PickBoundary(s *Session, tokensToRemove int) (Boundary, bool)
func (c *Consolidator) Consolidate(ctx context.Context, s *Session) (string, error)
```

Two more in `autocompact.go`:

```go
type AutoCompact struct {
    Sessions     *SessionManager
    Consolidator *Consolidator
    TTL          time.Duration
}

func NewAutoCompact(sm *SessionManager, c *Consolidator, ttl time.Duration) *AutoCompact
func (a *AutoCompact) CheckExpired(ctx context.Context, activeSessionKeys map[string]bool) []string
func (a *AutoCompact) Run(ctx context.Context, interval time.Duration, activeKeys func() map[string]bool)
```

`MemoryStore` gets one new method (the only s09→s10 carry-over change):

```go
func (m *MemoryStore) WriteSummary(sessionKey, summary string) error
func (m *MemoryStore) ReadSummary(sessionKey string) (string, error)  // bonus: round-trip
```

The summary lands at `<root>/<sessionKey>-summary.md`. The session key is sanitized (`:` and `/` and spaces become `_`) so it's a safe filename on every filesystem. Atomicity is via `tmp + os.Rename`, the same pattern as `WriteCursor`.

`Session.LastConsolidated` was a placeholder field in s06 — it finally gets read+written for real here. The pre-consolidation cursor is `s.LastConsolidated`; after `Consolidate` returns successfully, the cursor is `boundary.End`. Future history slices use this to drop the consolidated tail.

## How It Works

```ascii-anim frames=4
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  Session.Messages                                                │
│    [u, a, u, a, u, a, u, a, u, ...]                              │
│         ^                                                        │
│         LastConsolidated = 0                                     │
│                                                                  │
│  Consolidator.PickBoundary(s, tokensToRemove=N):                 │
│    walks idx = 0..len(Messages)                                  │
│    if idx > start && Role=="user" && !isToolResult(msg) &&       │
│       removed >= N:                                              │
│         last = Boundary{Start: 0, End: idx}                      │
│    returns the LATEST candidate                                  │
│                                                                  │
│  Consolidator.Consolidate(ctx, s):                               │
│    boundary, ok := PickBoundary(s, tokensToRemove)               │
│    if !ok { return "", ErrNoLegalBoundary }                      │
│                                                                  │
│    rendered := format(s.Messages[boundary.Start:boundary.End])   │
│    resp := provider.Chat(ctx, ChatReq{                           │
│        System: summarizationSystem,                              │
│        Messages: [{Role:"user", Content: rendered}],             │
│    })                                                            │
│    summary := resp.Content                                       │
│                                                                  │
│    memory.WriteSummary(s.Key, summary)                           │
│    memory.AppendHistory({Role:"system", Content:"[c] "+summary}) │
│    s.LastConsolidated = boundary.End                             │
│    return summary, nil                                           │
│                                                                  │
│  AutoCompact.CheckExpired(ctx, activeKeys):                      │
│    for key := range Sessions.sessions:                           │
│        if activeKeys[key] { continue }      // sacred: in-flight │
│        if a.archiving[key] { continue }     // already running   │
│        if now - sess.UpdatedAt <= TTL { continue }  // not idle  │
│        a.archiving[key] = true              // mark BEFORE go    │
│        go a.runOne(ctx, sess)               // fire-and-forget   │
│                                                                  │
│  AutoCompact.runOne(ctx, sess):                                  │
│    defer { delete(a.archiving, sess.Key) }                       │
│    summary, _ := Consolidator.Consolidate(ctx, sess)             │
│    a.summaries[sess.Key] = summary                               │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

Core ~30 lines (excerpted from [`agents/s10-consolidator-autocompact/consolidator.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s10-consolidator-autocompact/consolidator.go)):

```go
func (c *Consolidator) PickBoundary(s *Session, tokensToRemove int) (Boundary, bool) {
    if s == nil || tokensToRemove <= 0 { return Boundary{}, false }
    start := s.LastConsolidated
    if start >= len(s.Messages) { return Boundary{}, false }

    removed := 0
    var last Boundary
    found := false

    for idx := start; idx < len(s.Messages); idx++ {
        msg := s.Messages[idx]
        if idx > start && msg.Role == "user" && !isToolResultMessage(msg) && removed >= tokensToRemove {
            last = Boundary{Start: start, End: idx}
            found = true
        }
        removed += c.estimateMessageTokens(msg)
    }
    if !found { return Boundary{}, false }
    return last, true
}
```

**5 non-obvious points**:

1. **The boundary walk picks the LATEST candidate, not the FIRST.** Once `removed >= tokensToRemove`, every subsequent user message is also a valid stop. We pick the last one because it maximizes how much we drop in this round and minimizes how often we have to re-run. Upstream does the same thing (`last_boundary` is overwritten on each match, never returned early).

2. **`isToolResultMessage` is structural, not heuristic.** A user-role message whose every content block is type `tool_result` is a tool reply, not a fresh user prompt. Cutting between an `assistant` (with `tool_use`) and the corresponding `tool_result` orphans the call. The check is a direct walk of the content blocks — no string matching, no role guessing.

3. **Token estimation is approximate.** We ship `len(s)/4` as the default `TokenEst` because it's good enough for English text within a factor of 2 and trivial to read. Production callers swap in `pkoukk/tiktoken-go` (one line: `c.TokenEst = realTiktoken`). The boundary picker is conservative — it stops only when removed >= target, so the worst case of a 2x-off estimator is "we remove 2x as much as needed", which is fine.

4. **Summary persists as a separate file**, not as an in-place edit to `history.jsonl`. `history.jsonl` is the audit layer (append-only, never edited); the summary is the interpretation layer (LLM-generated, replaceable). Keeping them in different files preserves the audit trail — App. A's "memory as interpretation" pattern hinges on this.

5. **AutoCompact never blocks the agent loop.** `CheckExpired` returns the list of session keys it queued, then the goroutines run in the background. The `archiving` map is the idempotency guard — a second `CheckExpired` in flight while the first goroutine is still running won't double-queue. The sacred-active-keys filter is what prevents racing the bus on the same session's `Messages` slice.

## What Changed

```diff
  // s09/memory.go: MemoryStore had AppendHistory + ReadCursor + WriteCursor +
  //                ReadMemory + WriteMemory. Summary persistence didn't exist.
- type MemoryStore struct { root string }
- // (no WriteSummary method)

  // s10/memory.go: MemoryStore gains WriteSummary + ReadSummary. Everything
  //                else is unchanged.
+ func (m *MemoryStore) WriteSummary(sessionKey, summary string) error {
+     target := filepath.Join(m.root, summaryFilename(sessionKey))
+     return atomicWrite(target, []byte(summary))
+ }
+ func (m *MemoryStore) ReadSummary(sessionKey string) (string, error) { ... }
+ func summaryFilename(sessionKey string) string {
+     return strings.NewReplacer(":", "_", "/", "_", " ", "_").
+         Replace(sessionKey) + "-summary.md"
+ }

  // s09: Session.LastConsolidated was a placeholder; nothing read or wrote it.
- // s10: Consolidator.Consolidate sets s.LastConsolidated = boundary.End.
+ s.LastConsolidated = boundary.End
```

Brand-new types (the heart of s10):

```go
+ type Boundary struct { Start, End int }
+ type Consolidator struct { Provider Provider; TokenEst func(string) int;
+                            ConsolidationRatio float64; Memory *MemoryStore; Model string }
+ type AutoCompact struct { Sessions *SessionManager; Consolidator *Consolidator;
+                           TTL time.Duration; archiving map[string]bool;
+                           summaries map[string]string }
+ var ErrNoLegalBoundary = errors.New(...)
```

`Provider` / `Tool` / `Registry` / `Bus` / `SessionManager` / `Hook` / `ContextBuilder` / `SkillLoader` / `Runner` are all unchanged. `main.go` adds a `-autocompact-ttl` flag and spawns `autocompact.Run` in a goroutine.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s10-consolidator-autocompact

# Same single-turn behavior as s09; the new -autocompact-ttl flag is wired
# but doesn't fire on a single-turn run (no idle window).
go run . -workspace /tmp/agent-demo -autocompact-ttl 1m -v "Tokyo weather"
# [s10] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s10] loaded 2 skill(s) ...
# [s10] autocompact TTL: 1m0s
# [s10] system prompt: 612 chars; messages: 1
# [iter 0] -> bash {command:curl -s wttr.in/Tokyo?format=3}
# [iter 0] <- Tokyo: ☀ +18°C
# [s10] hook recorded 1 tool invocation(s): [bash]
# Tokyo: clear, 18°C.
```

Run the tests (race detector mandatory — AutoCompact spawns goroutines):

```bash
go test -count=1 -race -v ./...
```

9 tests should pass:

- `TestConsolidator_PickBoundary_LegalUserTurnOnly` — walks past assistants until it finds a real user turn;
- `TestConsolidator_NoLegalBoundary_ReturnsFalse` — short session + large `tokensToRemove` returns `ok=false` and `ErrNoLegalBoundary`;
- `TestConsolidator_Consolidate_PersistsSummary` — `<key>-summary.md` is written with the LLM's response;
- `TestConsolidator_AdvancesLastConsolidated` — `s.LastConsolidated` moves to `boundary.End`;
- `TestConsolidator_PickBoundary_SkipsToolResultMessages` — bonus invariant: tool_result-only user messages are NOT legal cut points;
- `TestAutoCompact_IdleSession_GetsCompacted` — happy path with TTL=50ms;
- `TestAutoCompact_ActiveSession_Skipped` — sessions in `activeKeys` are filtered out;
- `TestAutoCompact_AlreadyArchiving_Skipped` — back-to-back `CheckExpired` calls don't double-queue;
- `TestAutoCompact_RunLoop_StopsOnContextCancel` — Run terminates on ctx cancel.

To see the consolidator end-to-end without the LLM, run the unit tests in verbose mode and watch `cli_idle-summary.md` materialize in the test temp dir.

## Upstream Source Reading

s10 corresponds to two upstream files: `nanobot/agent/memory.py:800-1200` (the `Consolidator` class plus `pick_consolidation_boundary` and `archive`) and `nanobot/agent/autocompact.py` (the `AutoCompact` class). Together they're ~550 lines of Python; we translate the load-bearing 200 to Go.

Annotated version in [`upstream-readings/s10-consolidator-autocompact.py`](../../upstream-readings/s10-consolidator-autocompact.py).

```upstream:nanobot/agent/memory.py#L880-L905
def pick_consolidation_boundary(
    self,
    session: Session,
    tokens_to_remove: int,
) -> tuple[int, int] | None:
    """Pick a user-turn boundary that removes enough old prompt tokens."""
    start = session.last_consolidated
    if start >= len(session.messages) or tokens_to_remove <= 0:
        return None

    removed_tokens = 0
    last_boundary: tuple[int, int] | None = None
    for idx in range(start, len(session.messages)):
        message = session.messages[idx]
        if idx > start and message.get("role") == "user":
            last_boundary = (idx, removed_tokens)
            if removed_tokens >= tokens_to_remove:
                return last_boundary
        removed_tokens += estimate_message_tokens(message)

    return last_boundary
```

```upstream:nanobot/agent/autocompact.py#L60-L80
def check_expired(self, schedule_background: Callable[[Coroutine], None],
                  active_session_keys: Collection[str] = ()) -> None:
    """Schedule archival for idle sessions, skipping those with in-flight agent tasks."""
    now = datetime.now()
    for info in self.sessions.list_sessions():
        key = info.get("key", "")
        if not key or key in self._archiving:
            continue
        if key in active_session_keys:
            continue
        if self._is_expired(info.get("updated_at"), now):
            self._archiving.add(key)
            schedule_background(self._archive(key))
```

**5 cross-reading takeaways**:

- **Upstream returns `last_boundary` even on the not-enough-tokens path.** Our Go version returns `(Boundary{}, false)` when the threshold isn't met. The Python version returns the latest user-message tuple even when removed_tokens < target — the caller checks the second tuple element. Same effective behavior; we made the boolean explicit because Go doesn't have Python's truthiness flexibility around tuples.

- **Upstream uses `tiktoken.get_encoding("cl100k_base")` for real token counts.** We ship `len(s)/4` as the default. The trade-off is documented in the `TokenEst` field's comment: production swaps in `pkoukk/tiktoken-go`. The boundary picker is intentionally conservative so a 2x-off estimator just removes 2x as much as needed, which doesn't break correctness — it just over-compacts.

- **Upstream's AutoCompact has a `_summaries` cache** that survives across `check_expired` calls so the next `prepare_session` call can re-inject the most recent summary into the runtime context. We mirror this with `summaries map[string]string` and expose `LastSummary(key)` so `main.go` can print it. The full re-injection flow (where `prepare_session` reads the cache and prepends "Inactive for N minutes...") lands in s11, not s10.

- **Upstream's archive method falls back to `raw_archive`** when the LLM call fails. Raw_archive just dumps the messages to history.jsonl without summarization — a breadcrumb so the data isn't lost. We don't ship this fallback in s10; the test surface is small, the fallback is mostly defensive, and `ErrNoLegalBoundary` already conveys "couldn't compress, leave as-is". If you ship learn-nanobot in production, add it.

- **Upstream's `_RECENT_SUFFIX_MESSAGES = 8`** and the `retain_recent_legal_suffix` helper preserve the most recent 8 messages from being archived. The idea is that mid-conversation the user expects the agent to remember the immediate context perfectly — so we only compact what's already old. Our boundary picker walks forward from `LastConsolidated`, which gives the same effect for a long-tail session, but we don't enforce a "keep last N" minimum. If you find your agent forgetting too aggressively, add a `RetainSuffix int` field and skip messages within that window.

**For deeper reading**:

1. `nanobot/agent/memory.py:_consolidate_replay_overflow` — the replay-window consolidator. When a session's tail exceeds the configured "replay max messages" (different from token budget), this method runs a single archive pass over the overflow. We don't ship a replay-window cap in s10; for our purposes the token-based picker is enough.

2. `nanobot/agent/autocompact.py:_split_unconsolidated` — the helper that splits the un-consolidated tail into "stuff to archive" and "stuff to keep in the live message list". This is where `_RECENT_SUFFIX_MESSAGES` lands. Reading this gives you the production version of the keep-last-N invariant.

3. `nanobot/agent/memory.py:archive` and the `consolidator_archive.md` template — the actual summarization prompt. Our `summarizationSystem` const captures the intent (preserve facts, drop scratchpad) but is shorter; the upstream template is ~80 lines and tunes specifically for re-injection into the next system prompt.

---

**Next**: s11 — composing everything into the TurnState state machine.

Next: s11 — composing everything into the TurnState state machine.
