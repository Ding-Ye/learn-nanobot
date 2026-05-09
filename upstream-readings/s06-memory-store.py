# Source: HKUDS/nanobot · nanobot/agent/memory.py (lines 1-400, file-I/O portion)
# License: MIT
# Fetched: 2026-05-09 from main branch
#
# Teaching excerpt for learn-nanobot/s06-memory-store. The upstream file
# `memory.py` is ~1100 LOC and stacks three tiers in one module:
#
#   1. MemoryStore         — pure file I/O (this file: lines 1-400)
#   2. Consolidator        — LLM-assisted summarization (s10: lines 800-1200)
#   3. Dream               — high-level memory revision (App. A extension)
#
# This excerpt is just tier 1. We carry over MemoryStore.__init__,
# append_history, read/write of MEMORY/SOUL/USER, and cursor handling.
# The legacy HISTORY.md migration code is annotated but kept brief — it's
# upgrade plumbing, not core mechanism.

# ────────────────────────────────────────────────────────────────────
# Part 1. MemoryStore.__init__ — the four-file split
# ────────────────────────────────────────────────────────────────────

class MemoryStore:
    """Pure file I/O for memory files: MEMORY.md, history.jsonl, SOUL.md, USER.md."""

    _DEFAULT_MAX_HISTORY = 1000
    # Regex constants for legacy migration only — see Part 4.
    _LEGACY_ENTRY_START_RE = re.compile(r"^\[(\d{4}-\d{2}-\d{2}[^\]]*)\]\s*")
    _LEGACY_TIMESTAMP_RE = re.compile(r"^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2})\]\s*")

    def __init__(self, workspace: Path, max_history_entries: int = _DEFAULT_MAX_HISTORY):
        # `workspace` is the agent's root directory:
        #   ~/.nanobot/workspace/agents/<name>/   (upstream)
        #   ~/.learn-nanobot/agents/<name>/       (learn-nanobot s06)
        self.workspace = workspace
        self.max_history_entries = max_history_entries

        # The four files. Upstream nests three of them under a `memory/`
        # subdirectory; learn-nanobot/s06 puts them all under <root>/ flat
        # because we don't have the `MEMORY.md` ↔ git correlation that
        # upstream's GitStore tracks.
        self.memory_dir = ensure_dir(workspace / "memory")
        self.memory_file = self.memory_dir / "MEMORY.md"
        self.history_file = self.memory_dir / "history.jsonl"
        self.legacy_history_file = self.memory_dir / "HISTORY.md"  # pre-jsonl format
        self.soul_file = workspace / "SOUL.md"   # NOT under memory/
        self.user_file = workspace / "USER.md"   # NOT under memory/

        # Two cursor files. The "main" cursor advances on every successful
        # append_history; .dream_cursor advances only when Dream completes
        # an interpretation pass. Decoupling them lets s10's Consolidator
        # and App. A's Dream operate independently.
        self._cursor_file = self.memory_dir / ".cursor"
        self._dream_cursor_file = self.memory_dir / ".dream_cursor"

        # GitStore wraps SOUL/USER/MEMORY in a real git repository — every
        # Dream cycle is a commit, every Consolidator update is a commit,
        # so `git log` shows the evolution of the agent's beliefs.
        # learn-nanobot/s06 OMITS this. App. A discusses how to re-add it.
        self._git = GitStore(workspace, tracked_files=[
            "SOUL.md", "USER.md", "memory/MEMORY.md", "memory/.dream_cursor",
        ])

        # One-shot upgrade from the pre-jsonl `HISTORY.md` flat format to
        # the new jsonl. learn-nanobot has no legacy data so we skip this.
        self._maybe_migrate_legacy_history()


# ────────────────────────────────────────────────────────────────────
# Part 2. The append-only path: append_history + cursor handling
# ────────────────────────────────────────────────────────────────────

def append_history(self, entry: str, *, max_chars: int | None = None) -> int:
    """Append *entry* to history.jsonl and return its auto-incrementing cursor.

    learn-nanobot/s06's AppendHistory mirrors this: opens with O_APPEND |
    O_CREATE | O_WRONLY, writes the JSON line + '\\n', then writes the
    new cursor to .cursor via tmp+rename. Two important differences:

      - Upstream input is a string + dict-style record `{cursor, timestamp,
        content}`. learn-nanobot uses a typed `HistoryEntry` struct with
        `{ts, role, content, tools_used?, usage?}` — same shape, more
        Go-idiomatic.
      - Upstream `strip_think(raw)` removes `<think>...</think>` blocks
        before persisting (so the next turn's LLM doesn't echo its own
        internal monologue back). learn-nanobot delegates that to s08's
        ContextBuilder (anti-pattern #4: single sanitize boundary).
    """
    limit = max_chars if max_chars is not None else _HISTORY_ENTRY_HARD_CAP

    # Compute the next cursor. Three-tier fallback:
    #   1. Read .cursor file (cheap).
    #   2. Read tail of history.jsonl, take last entry's cursor (medium).
    #   3. Scan whole file, take max(cursor) (slow).
    # learn-nanobot/s06 collapses this to "count lines" — adequate at our
    # file sizes; if you ever process millions of turns, port back the
    # fast path.
    cursor = self._next_cursor()
    ts = datetime.now().strftime("%Y-%m-%d %H:%M")
    raw = entry.rstrip()

    # Defensive cap. learn-nanobot/s06 doesn't enforce a per-entry cap;
    # callers are responsible. If you want this in your Go port, add a
    # `MaxEntryBytes int` field to MemoryStore and check `len(enc)` against
    # it inside AppendHistory.
    if len(raw) > limit:
        raw = truncate_text(raw, limit)

    # Upstream-only: strip <think> blocks before persisting.
    content = strip_think(raw)

    record = {"cursor": cursor, "timestamp": ts, "content": content}
    with open(self.history_file, "a", encoding="utf-8") as f:
        f.write(json.dumps(record, ensure_ascii=False) + "\n")
    self._cursor_file.write_text(str(cursor), encoding="utf-8")
    return cursor


def _next_cursor(self) -> int:
    """Read the current cursor counter and return the next value."""
    # Fast path: trust .cursor.
    if self._cursor_file.exists():
        with suppress(ValueError, OSError):
            return int(self._cursor_file.read_text(encoding="utf-8").strip()) + 1
    # Medium path: read just the last line via reverse seek.
    last = self._read_last_entry() or {}
    cursor = self._valid_cursor(last.get("cursor"))
    if cursor is not None:
        return cursor + 1
    # Slow path: full scan.
    return max((c for _, c in self._iter_valid_entries()), default=0) + 1


# ────────────────────────────────────────────────────────────────────
# Part 3. The atomic-replace path: read/write MEMORY/SOUL/USER
# ────────────────────────────────────────────────────────────────────

# These are deliberately tiny. "atomic replace" only really matters for
# `_write_entries` (the migration path) — for normal MEMORY/SOUL/USER
# writes upstream uses Path.write_text() directly, which on POSIX writes
# to the same file in-place via open()/write()/close(). This is NOT
# strictly atomic at the filesystem level; learn-nanobot/s06 is more
# conservative and uses tmp+rename for ALL writes, which IS atomic.
#
# This is a small but real upgrade in our Go port: we made the safer
# choice everywhere, because in Go writing the helper once is cheap.

@staticmethod
def read_file(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return ""    # ← learn-nanobot/s06's ReadMemory matches this contract

def read_memory(self) -> str:
    return self.read_file(self.memory_file)

def write_memory(self, content: str) -> None:
    # NOTE: upstream uses naïve write_text; learn-nanobot/s06 uses
    # atomicWrite (tmp + os.Rename) for safer crash semantics.
    self.memory_file.write_text(content, encoding="utf-8")

def read_soul(self) -> str:
    return self.read_file(self.soul_file)

def write_soul(self, content: str) -> None:
    self.soul_file.write_text(content, encoding="utf-8")

def read_user(self) -> str:
    return self.read_file(self.user_file)

def write_user(self, content: str) -> None:
    self.user_file.write_text(content, encoding="utf-8")


# ────────────────────────────────────────────────────────────────────
# Part 4. Atomic _write_entries — the closest upstream analog of our
#         atomicWrite helper. This is used for the legacy-migration
#         rewrite path and for compact_history (pruning oldest entries).
# ────────────────────────────────────────────────────────────────────

def _write_entries(self, entries: list[dict[str, Any]]) -> None:
    """Overwrite history.jsonl with the given entries (atomic write)."""
    tmp_path = self.history_file.with_suffix(self.history_file.suffix + ".tmp")
    try:
        with open(tmp_path, "w", encoding="utf-8") as f:
            for entry in entries:
                f.write(json.dumps(entry, ensure_ascii=False) + "\n")
            f.flush()
            os.fsync(f.fileno())   # learn-nanobot/s06 keeps this (best-effort)
        os.replace(tmp_path, self.history_file)
        # ↑ POSIX atomic-rename. Same idea as our atomicWrite.

        # fsync the directory so the rename is durable. learn-nanobot/s06
        # SKIPS this (Windows can't open a directory; macOS / Linux it's
        # not strictly required for our threat model).
        with suppress(PermissionError):
            fd = os.open(str(self.history_file.parent), os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
    except BaseException:
        tmp_path.unlink(missing_ok=True)
        raise


# ────────────────────────────────────────────────────────────────────
# Part 5. Dream cursor — preview of App. A
# ────────────────────────────────────────────────────────────────────
#
# learn-nanobot/s06 only models ONE cursor (`.cursor`). Upstream models
# TWO. The reason becomes clear in App. A:
#
#   .cursor          ← Consolidator advances after each summary.
#                       Says: "I have summarized history up through line N."
#
#   .dream_cursor    ← Dream advances after each interpretation pass.
#                       Says: "I have re-read history through line N to
#                       refresh my long-term beliefs in MEMORY/SOUL/USER."
#
# These two pipelines are independent: Consolidator runs cheap+often,
# Dream runs expensive+rare (e.g. nightly). Modeling both as separate
# cursors lets each track its own progress.
#
# learn-nanobot only ships `.cursor` because s10 only ships the
# Consolidator. App. A's extension exercise is to add `.dream_cursor`
# and a Dream pass.

def get_last_dream_cursor(self) -> int:
    if self._dream_cursor_file.exists():
        with suppress(ValueError, OSError):
            return int(self._dream_cursor_file.read_text(encoding="utf-8").strip())
    return 0

def set_last_dream_cursor(self, cursor: int) -> None:
    self._dream_cursor_file.write_text(str(cursor), encoding="utf-8")


# ────────────────────────────────────────────────────────────────────
# What this excerpt teaches (vs. learn-nanobot/s06)
# ────────────────────────────────────────────────────────────────────
#
# 1. Four files, two write semantics. Same in both languages.
#
# 2. Upstream uses "memory/" subdirectory; we flatten. Cosmetic.
#
# 3. Upstream's `strip_think` runs at the I/O boundary. learn-nanobot
#    moves it to s08 (ContextBuilder) — single sanitize point.
#
# 4. Upstream has GitStore for SOUL/USER/MEMORY. learn-nanobot omits.
#    App. A extension: add it back.
#
# 5. Upstream has TWO cursors (.cursor + .dream_cursor). learn-nanobot
#    s06 has one; App. A adds the second.
#
# 6. Upstream's atomic write only protects `_write_entries`; ordinary
#    MEMORY/SOUL/USER writes go through naïve write_text. learn-nanobot's
#    atomicWrite is universal — a small but real upgrade.
#
# 7. Concurrency: upstream is single-event-loop asyncio (no threads inside
#    one process). learn-nanobot guards AppendHistory with sync.Mutex
#    because Go's bus is multi-goroutine. The race-detector test
#    (TestMemoryStore_AppendHistory_Concurrent_ConsistentLines) verifies
#    the guard works.
#
# Reading map:
#   - nanobot/agent/memory.py:1-400   — this excerpt (file I/O)
#   - nanobot/agent/memory.py:400-800 — context injection helpers
#   - nanobot/agent/memory.py:800-1200— Consolidator (s10's territory)
#   - nanobot/utils/gitstore.py       — GitStore (App. A extension)
