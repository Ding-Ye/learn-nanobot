---
title: "s06 · Memory store (file I/O)"
chapter: 6
slug: s06-memory-store
est_read_min: 22
---

# s06 · Memory store (file I/O)

> What this teaches: wire s05's in-memory SessionManager to disk. One file is append-only (`history.jsonl` — the truth); three are atomic-replace (`MEMORY.md` / `SOUL.md` / `USER.md` — the interpretation); a separate `.cursor` lets s10's Consolidator track "how far have I summarized".

---

## Problem

s05's SessionManager only lives in memory. Once the process exits, every conversation history is gone. A real agent has to:

- **Survive process restarts.** Run the CLI today, run it tomorrow, the agent still remembers your name.
- **Never lose the truth across crashes.** Even if the LLM's last reply was wrong, the raw record must survive — it's the only way Dream (App. A) can re-read history later and revise its beliefs.
- **Allow high-level rewrites.** Things like "user prefers concise answers", "I'm Sam in SF" are **long-term facts**. They can't be a flat append-only stream — they need a **curated, replaceable** distilled version that fits in the system prompt.

Upstream nanobot's answer is classic Unix-y: **all state is files**. `nanobot/agent/memory.py` defines a `MemoryStore` that maintains four separate files, each with its own write semantics.

## Solution

A new file `memory.go` introduces two types and one constant:

```go
// memory.go
type HistoryEntry struct {
    TS        time.Time `json:"ts"`
    Role      string    `json:"role"`
    Content   string    `json:"content"`
    ToolsUsed []string  `json:"tools_used,omitempty"`
    Usage     Usage     `json:"usage,omitempty"`
}

type MemoryStore struct {
    root     string       // <root>/ holds all state for one agent
    appendMu sync.Mutex   // belt-and-braces around O_APPEND
}

var BootstrapFiles = []string{"AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md"}
//                              s08's ContextBuilder reads all four

func NewMemoryStore(root string) (*MemoryStore, error)               // mkdir -p
func (m *MemoryStore) AppendHistory(e HistoryEntry) (cursor int, err) // append-only
func (m *MemoryStore) ReadCursor() (int, error)                      // missing → 0
func (m *MemoryStore) WriteCursor(c int) error                       // tmp + rename
func (m *MemoryStore) ReadMemory(filename string) (string, error)     // missing → ""
func (m *MemoryStore) WriteMemory(filename, body string) error        // tmp + rename
```

Four files, two write semantics:

| File | Write semantics | Role | Writer |
|---|---|---|---|
| `history.jsonl` | append-only, one `HistoryEntry` per line | Truth, audit, replay | s06's main (after each turn) |
| `.cursor` | atomic replace (tmp + os.Rename) | "I've successfully recorded N lines" | inside AppendHistory; advanced by s10 |
| `MEMORY.md` | atomic replace | LLM's interpretation of long-term facts | s10 (Consolidator) / App. A (Dream) |
| `SOUL.md` | atomic replace | Agent persona | App. A (Dream) |
| `USER.md` | atomic replace | User preferences as the agent sees them | App. A (Dream) |

`Session.LastConsolidated` was a placeholder in s05; from s06 onward it is **filled in for real**: every successful `AppendHistory` lands a new cursor in `.cursor`, and `Session.LastConsolidated` will be advanced by s10 — it represents "summary progress", **not** "log length", which is why the two have to be separate concepts.

## How It Works

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  Bus.sessionLoop:                                                │
│    result, _ := runner.Run(ctx, spec)                            │
│    out := OutboundMessage{Text: result.FinalContent, Result: result, ...}
│    b.out <- out                                                  │
│                                                                  │
│  main:                                                           │
│    out := <-bus.Out()                                            │
│    mem.AppendHistory({Role:"user",      Content: prompt})         │
│    mem.AppendHistory({Role:"assistant", Content: out.Text,        │
│                       ToolsUsed: out.Result.ToolsUsed,            │
│                       Usage: out.Result.Usage})                   │
│    fmt.Println(out.Text)                                          │
│                                                                  │
│  Inside AppendHistory (O_APPEND + mutex + .cursor write):        │
│    m.appendMu.Lock()                                              │
│    f := os.OpenFile(history.jsonl, O_APPEND|O_CREATE|O_WRONLY)    │
│    f.Write(json.Marshal(entry) + "\n")                            │
│    f.Close()                                                      │
│    cursor := count_lines(history.jsonl)                           │
│    atomicWrite(.cursor, str(cursor))   // tmp + os.Rename          │
│    m.appendMu.Unlock()                                            │
│                                                                  │
│  ┌──────────── Append-only vs Atomic Replace ────────────────┐    │
│  │                                                            │    │
│  │   history.jsonl  ──  append-only   ──  raw truth, never lost│    │
│  │                                                            │    │
│  │   MEMORY.md      ──  atomic replace ──  LLM's reading, can be revised│    │
│  │   SOUL.md        ──  atomic replace ──  persona; Dream's domain│
│  │   USER.md        ──  atomic replace ──  user model, revised│    │
│  │   .cursor        ──  atomic replace ──  small int, no history│    │
│  │                                                            │    │
│  │  Two semantics = two roles:                                │    │
│  │   "record a fact" → append; "revise an interpretation" →   │    │
│  │   replace                                                   │    │
│  └────────────────────────────────────────────────────────────┘    │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

The core ~25 lines (excerpt from [`agents/s06-memory-store/memory.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s06-memory-store/memory.go)):

```go
func (m *MemoryStore) AppendHistory(entry HistoryEntry) (int, error) {
    m.appendMu.Lock()
    defer m.appendMu.Unlock()

    if entry.TS.IsZero() {
        entry.TS = time.Now().UTC()
    }
    historyPath := filepath.Join(m.root, "history.jsonl")
    f, err := os.OpenFile(historyPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
    if err != nil { return 0, err }

    enc, _ := json.Marshal(entry)
    enc = append(enc, '\n')
    if _, err := f.Write(enc); err != nil { f.Close(); return 0, err }
    if err := f.Close(); err != nil { return 0, err }

    cursor, err := m.countLines(historyPath)   // re-count cheap; we won't see PB-sized files
    if err != nil { return 0, err }
    if err := m.writeCursorAtomic(cursor); err != nil { return 0, err }
    return cursor, nil
}

// atomic replace — tmp file in the same dir + os.Rename. POSIX guarantees
// rename within a filesystem is atomic.
func atomicWrite(target string, payload []byte) error {
    dir := filepath.Dir(target)
    tmp, err := os.CreateTemp(dir, filepath.Base(target)+".tmp-*")
    if err != nil { return err }
    if _, err := tmp.Write(payload); err != nil { _ = tmp.Close(); _ = os.Remove(tmp.Name()); return err }
    _ = tmp.Sync()
    if err := tmp.Close(); err != nil { _ = os.Remove(tmp.Name()); return err }
    return os.Rename(tmp.Name(), target)
}
```

**4 non-obvious points**:

1. **`O_APPEND` plus a mutex is belt-and-braces.** POSIX guarantees `O_APPEND` writes ≤ `PIPE_BUF` (typically 4 KB) are atomic relative to other O_APPEND writes — but our JSON lines can exceed that, especially with large tool_result content. `appendMu` covers the gap; the race detector will catch any leak.
2. **`os.Rename` is the atomicity primitive.** POSIX says rename **within the same filesystem** is atomic. We create the tmp file in the **same directory** (via `os.CreateTemp(dir, ...)`) so we never cross a filesystem boundary — that's the critical detail; cross-filesystem rename degrades to copy+unlink and loses atomicity.
3. **`.cursor` is not derived data; it's an independent source-of-truth.** In principle cursor = `wc -l history.jsonl`, but **decoupling them matters**: in s10 the cursor only advances **when a summary is successful**, *not* on every append. "How long is the log" ≠ "how far have I processed it" — they only diverge in s10, and decoupling them today is what makes that future work easy.
4. **`ReadMemory` returns `""` for a missing file, not an error.** This is the contract s08's ContextBuilder relies on: a fresh agent has no `MEMORY.md`, and the caller shouldn't have to special-case it. Real I/O errors (permissions, etc.) still propagate.

## What Changed (vs. s05)

```diff
  // s05/main.go: one turn, then exit; all Session.Messages get GC'd
- bus.Send(InboundMessage{SessionKey: *sessionKey, UserText: prompt, ID: "cli-1"})
- out := <-bus.Out()
- fmt.Println(out.Text)
- bus.Stop()

  // s06/main.go: a -workspace flag and MemoryStore; turn lands on disk
+ workspace := flag.String("workspace", "", "workspace root (default ~/.learn-nanobot/agents/default)")
+ root, _ := resolveWorkspace(*workspace)
+ mem, _ := NewMemoryStore(root)
  ...
  bus.Send(...)
  out := <-bus.Out()
+ mem.AppendHistory(HistoryEntry{Role: "user",      Content: prompt})
+ mem.AppendHistory(HistoryEntry{Role: "assistant", Content: out.Text,
+                                ToolsUsed: out.Result.ToolsUsed,
+                                Usage:     out.Result.Usage})
  fmt.Println(out.Text)
  bus.Stop()
```

New types (the s06 spine):

```go
+ type HistoryEntry struct {
+     TS        time.Time `json:"ts"`
+     Role      string    `json:"role"`
+     Content   string    `json:"content"`
+     ToolsUsed []string  `json:"tools_used,omitempty"`
+     Usage     Usage     `json:"usage,omitempty"`
+ }
+ type MemoryStore struct { root string; appendMu sync.Mutex }
+ var BootstrapFiles = []string{"AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md"}
```

`Bus.OutboundMessage` gets one new field:

```diff
  type OutboundMessage struct {
      SessionKey    string
      Text          string
      Err           error
      CorrelationID string
+     Result        AgentRunResult  // main reads Usage / ToolsUsed for persistence
  }
```

`Session.LastConsolidated` has existed since s05, but **was never written before s06**. From this chapter on, the field corresponds to "the cursor up to which the Consolidator has summarized" — s06 doesn't write it (that's s10's job), but **the semantics finally exist** here.

The `Provider` / `Tool` / `Registry` / `Runner` / `Session` / `Bus` core APIs are unchanged.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s06-memory-store

# Run once, write into a temp workspace
go run . -workspace /tmp/agent-demo "list .go files in this directory"

# Inspect what landed on disk
ls -la /tmp/agent-demo
# .cursor + history.jsonl

cat /tmp/agent-demo/.cursor
# 2  — this turn wrote two entries (user + assistant)

head -1 /tmp/agent-demo/history.jsonl | python3 -m json.tool
# {"ts": "2026-05-09T...", "role": "user", "content": "list .go files..."}
```

Run the tests (race detector required):

```bash
go test -count=1 -race -v ./...
```

Expected:

```
=== RUN   TestMemoryStore_AppendHistory_AppendsLine
--- PASS: TestMemoryStore_AppendHistory_AppendsLine
=== RUN   TestMemoryStore_ReadMemory_DefaultsWhenMissing
--- PASS: TestMemoryStore_ReadMemory_DefaultsWhenMissing
=== RUN   TestMemoryStore_WriteMemory_AtomicReplace
--- PASS: TestMemoryStore_WriteMemory_AtomicReplace
=== RUN   TestMemoryStore_Cursor_RoundTrip
--- PASS: TestMemoryStore_Cursor_RoundTrip
=== RUN   TestMemoryStore_AppendHistory_Concurrent_ConsistentLines
--- PASS: TestMemoryStore_AppendHistory_Concurrent_ConsistentLines
PASS
```

`Concurrent_ConsistentLines` is the most pedagogically loaded test: 10 goroutines append concurrently; the final line count must be 10 and every line must parse as JSON. If you comment out `appendMu.Lock()` and bump the entry size to a few dozen KB, the race detector will fire — that's the practical demonstration that "O_APPEND atomicity stops at PIPE_BUF" is a real boundary, not a textbook footnote.

To experience "history persists across processes":

```bash
# First run: write turn 1
go run . -workspace /tmp/demo "my name is Sam"
cat /tmp/demo/.cursor   # 2

# Second run: append turn 2; cursor goes to 4
go run . -workspace /tmp/demo "the weather is great today"
cat /tmp/demo/.cursor   # 4
wc -l /tmp/demo/history.jsonl  # 4 lines
```

Note: the SessionManager **does not yet rebuild in-memory `Messages` from `history.jsonl`** — the LLM in run 2 has no idea what was said in run 1. That's intentional: s06 only teaches file I/O. s11's Loop will own the RESTORE state.

## Upstream Source Reading

s06 maps to `nanobot/agent/memory.py:1-400` — the file I/O portion. The Consolidator (lines 800–1200) is s10's territory; Dream (further down) is App. A's extension exercise.

Full annotated excerpt at [`upstream-readings/s06-memory-store.py`](../../upstream-readings/s06-memory-store.py).

```upstream:nanobot/agent/memory.py#L41-L67
class MemoryStore:
    """Pure file I/O for memory files: MEMORY.md, history.jsonl, SOUL.md, USER.md."""

    _DEFAULT_MAX_HISTORY = 1000

    def __init__(self, workspace: Path, max_history_entries: int = _DEFAULT_MAX_HISTORY):
        self.workspace = workspace
        self.max_history_entries = max_history_entries
        self.memory_dir = ensure_dir(workspace / "memory")
        self.memory_file = self.memory_dir / "MEMORY.md"
        self.history_file = self.memory_dir / "history.jsonl"
        self.legacy_history_file = self.memory_dir / "HISTORY.md"
        self.soul_file = workspace / "SOUL.md"
        self.user_file = workspace / "USER.md"
        self._cursor_file = self.memory_dir / ".cursor"
        self._dream_cursor_file = self.memory_dir / ".dream_cursor"
        self._git = GitStore(workspace, tracked_files=[
            "SOUL.md", "USER.md", "memory/MEMORY.md", "memory/.dream_cursor",
        ])
        self._maybe_migrate_legacy_history()
```

```upstream:nanobot/agent/memory.py#L235-L275
def append_history(self, entry: str, *, max_chars: int | None = None) -> int:
    limit = max_chars if max_chars is not None else _HISTORY_ENTRY_HARD_CAP
    cursor = self._next_cursor()
    ts = datetime.now().strftime("%Y-%m-%d %H:%M")
    raw = entry.rstrip()
    if len(raw) > limit:
        raw = truncate_text(raw, limit)
    content = strip_think(raw)
    record = {"cursor": cursor, "timestamp": ts, "content": content}
    with open(self.history_file, "a", encoding="utf-8") as f:
        f.write(json.dumps(record, ensure_ascii=False) + "\n")
    self._cursor_file.write_text(str(cursor), encoding="utf-8")
    return cursor
```

**5 reading notes**:

- **Upstream has `git` integration; we don't.** `MemoryStore.git` is a `GitStore` that auto-commits SOUL/USER/MEMORY into the workspace as a real git repository — this is nanobot's killer "memory is auditable" feature (every Dream cycle is one commit; `git log` shows the agent's belief evolution over time). We don't teach it in s06: understand the core mechanism first, then layer git on top. App. A discusses this extension.
- **Upstream's `append_history` takes a string** with `"cursor"` baked into the dict; our Go version takes a `HistoryEntry` struct. The reason is idiomatic — Go's `encoding/json` is happier with typed structs. Functionally equivalent.
- **Upstream's `_next_cursor` has fast/medium/slow paths**: trust the `.cursor` file (fast), else read just the last line's cursor field (medium), else scan the whole file and take max (slow). We collapsed it to "`countLines(history.jsonl)`" — files won't reach PB-scale and simplicity wins. If your agent produces millions of turns a day, port the fast path back.
- **Upstream's `strip_think(raw)` step is missing here.** It strips `<think>...</think>` reasoning blocks before persisting, so the next turn's LLM doesn't see its own previous "inner monologue" being echoed back. We'll handle that in s08's ContextBuilder (anti-pattern #4: a single sanitize boundary). s06 stays as raw I/O.
- **Upstream has legacy migration (`HISTORY.md` → `history.jsonl`).** That's for upstream's existing users. learn-nanobot has no legacy users, so we drop it. When you read the upstream code and see `_maybe_migrate_legacy_history`, recognize it as historical baggage and move on.

**Read further**:

1. `nanobot/agent/memory.py:367-390` — `_write_entries` is upstream's atomic-write (with `os.fsync` on both file and directory). We simplified: file fsync only, no directory fsync, because directory fsync isn't strictly required on macOS / Linux for normal-user scenarios (and fails on Windows).
2. `nanobot/agent/memory.py:393-405` — `.dream_cursor` is a *second* cursor. Dream advances it; the Consolidator advances `.cursor`. Two cursors = two independent "how far have I processed" — both s10 and App. A use this.
3. `nanobot/utils/gitstore.py` — implementation of GitStore. If you want to add git-backed memory in Phase G, this is the reference.

---

**Next**: s07 teaches the agent skills. A `SkillsLoader` walks the workspace + builtin skill directories, parses YAML frontmatter, checks `requires.bins` / `requires.env`, and concatenates available skill bodies into the system prompt. The `MemoryStore` and `Runner` APIs don't change.
