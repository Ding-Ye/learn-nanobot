# Source: HKUDS/nanobot · nanobot/session/manager.py + nanobot/bus/queue.py + nanobot/bus/events.py
# License: MIT
# Fetched: 2026-05-09 from main branch
#
# Teaching excerpt for learn-nanobot/s05-session-bus. The upstream code
# splits "what" (data classes) from "where it's serialized" — the lock
# itself lives in nanobot/agent/loop.py, not in the bus or session files.
# This excerpt stitches the three pieces together so the picture is whole:
#   1. Session dataclass + SessionManager.get_or_create  (manager.py)
#   2. MessageBus + InboundMessage / OutboundMessage     (queue.py + events.py)
#   3. Where the per-session asyncio.Lock actually lives (loop.py, snippet)
#
# Skipped: load/save/repair (s06 territory), enforce_file_cap (s10 with
# Consolidator), get_history's full token-budget pass (also s10).

# ────────────────────────────────────────────────────────────────────
# Part 1. Session dataclass — nanobot/session/manager.py
# ────────────────────────────────────────────────────────────────────

@dataclass
class Session:
    """A conversation session."""

    # The key is logically <channel>:<chat_id>, e.g. "telegram:42",
    # "slack:C123ABC", "cli:direct". The Bus uses it to look up which
    # goroutine to dispatch to.
    key: str

    # The full message log. Upstream stores `dict[str, Any]` so it can
    # carry tool_calls / tool_call_id / reasoning_content / etc. as
    # extra keys; learn-nanobot's Go translation uses []Message with a
    # ContentBlock list — same idea, typed.
    messages: list[dict[str, Any]] = field(default_factory=list)

    # Timestamps are useful for telemetry + the autocompact TTL check
    # (s10). UpdatedAt is bumped on every add_message; CreatedAt is
    # set once.
    created_at: datetime = field(default_factory=datetime.now)
    updated_at: datetime = field(default_factory=datetime.now)

    # Channel-specific bag. Upstream stores e.g. the Telegram chat_id
    # int, last_seen username. We don't model this in s05.
    metadata: dict[str, Any] = field(default_factory=dict)

    # The Consolidator (s10) advances this. Anything before the cursor
    # has been LLM-summarized into the MemoryStore; get_history returns
    # only messages[last_consolidated:].
    last_consolidated: int = 0

    def add_message(self, role: str, content: str, **kwargs: Any) -> None:
        """Add a message to the session.

        Each message also gets a fresh ISO timestamp so get_history can
        return it back to the model later (for relative-time reasoning).
        learn-nanobot/s05's Session.Append doesn't stamp messages — the
        equivalent will live on Hook (s09) or RuntimeMeta (s08) instead.
        """
        msg = {
            "role": role,
            "content": content,
            "timestamp": datetime.now().isoformat(),
            **kwargs,
        }
        self.messages.append(msg)
        self.updated_at = datetime.now()


# ────────────────────────────────────────────────────────────────────
# Part 2. SessionManager.get_or_create — the cache contract
# ────────────────────────────────────────────────────────────────────

class SessionManager:
    """Manages conversation sessions.

    Sessions are stored as JSONL files in the sessions directory.
    learn-nanobot/s05 ports only the in-memory cache half; s06 will add
    the file half on top.
    """

    def __init__(self, workspace: Path):
        self.workspace = workspace
        self.sessions_dir = ensure_dir(self.workspace / "sessions")
        # ↑ learn-nanobot/s05 doesn't have this — no disk in this chapter.
        self._cache: dict[str, Session] = {}

    def get_or_create(self, key: str) -> Session:
        """Get an existing session or create a new one.

        learn-nanobot/s05's GetOrCreate has the same contract but uses
        a sync.RWMutex with double-checked locking — cheaper for the
        common "already cached" path under contention.
        """
        # Cache hit?
        if key in self._cache:
            return self._cache[key]

        # Miss: try disk (s06 in our curriculum), or create fresh.
        session = self._load(key)
        if session is None:
            session = Session(key=key)

        self._cache[key] = session
        return session


# ────────────────────────────────────────────────────────────────────
# Part 3. MessageBus — nanobot/bus/queue.py
# ────────────────────────────────────────────────────────────────────

class MessageBus:
    """Async message bus that decouples chat channels from the agent.

    Channels push messages to the inbound queue, and the agent processes
    them and pushes responses to the outbound queue.

    NOTE for learn-nanobot/s05 readers: this Python class is tiny — it's
    JUST two asyncio.Queues. Per-session serialization is NOT here; it
    lives in agent/loop.py (see Part 5 below). We fuse the two concerns
    into one Go Bus because chan + goroutine is naturally both.
    """

    def __init__(self):
        # inbound: produced by channel adapters, consumed by AgentLoop.
        self.inbound: asyncio.Queue[InboundMessage] = asyncio.Queue()
        # outbound: produced by AgentLoop, consumed by channel adapters.
        self.outbound: asyncio.Queue[OutboundMessage] = asyncio.Queue()

    async def publish_inbound(self, msg: InboundMessage) -> None:
        await self.inbound.put(msg)

    async def consume_inbound(self) -> InboundMessage:
        return await self.inbound.get()

    async def publish_outbound(self, msg: OutboundMessage) -> None:
        await self.outbound.put(msg)

    async def consume_outbound(self) -> OutboundMessage:
        return await self.outbound.get()


# ────────────────────────────────────────────────────────────────────
# Part 4. Inbound / Outbound events — nanobot/bus/events.py
# ────────────────────────────────────────────────────────────────────

@dataclass
class InboundMessage:
    """Message received from a chat channel."""

    channel: str       # e.g. "telegram" / "discord" / "slack" / "cli"
    sender_id: str     # user identifier within that channel
    chat_id: str       # chat/channel identifier within that channel
    content: str       # message text

    timestamp: datetime = field(default_factory=datetime.now)
    media: list[str] = field(default_factory=list)        # image URLs etc.
    metadata: dict[str, Any] = field(default_factory=dict)
    session_key_override: str | None = None  # for thread-scoped sessions

    @property
    def session_key(self) -> str:
        """Unique key for session identification.

        learn-nanobot/s05 collapses channel+chat_id into a single string
        passed by the caller. The override exists for cases like Slack
        threads (one chat_id, many threaded sub-conversations).
        """
        return self.session_key_override or f"{self.channel}:{self.chat_id}"


@dataclass
class OutboundMessage:
    """Message to send to a chat channel."""

    channel: str
    chat_id: str
    content: str
    reply_to: str | None = None
    media: list[str] = field(default_factory=list)
    metadata: dict[str, Any] = field(default_factory=dict)
    buttons: list[list[str]] = field(default_factory=list)
    # learn-nanobot/s05's OutboundMessage adds a CorrelationID so tests
    # can match request → reply for serial-ordering assertions.


# ────────────────────────────────────────────────────────────────────
# Part 5. Where the per-session lock actually lives (loop.py snippet)
# ────────────────────────────────────────────────────────────────────
#
# This is the snippet learn-nanobot/s05's Go Bus replicates structurally.
# Upstream:
#
#     class AgentLoop:
#         def __init__(self, ...):
#             self._session_locks: dict[str, asyncio.Lock] = {}
#
#         async def _process_inbound(self, msg: InboundMessage):
#             key = msg.session_key
#             # First-touch creates the lock; subsequent calls find the
#             # same one. asyncio.Lock is single-event-loop, no thread
#             # safety needed (Python's asyncio is single-threaded).
#             lock = self._session_locks.setdefault(key, asyncio.Lock())
#             async with lock:
#                 session = self.sessions.get_or_create(key)
#                 # ... call AgentRunner with session history ...
#
# Go translation — see learn-nanobot/s05/bus.go::Bus.Send + sessionLoop:
#
#     b.mu.Lock()                            # protects the b.in MAP
#     ch, ok := b.in[msg.SessionKey]
#     if !ok {
#         ch = make(chan InboundMessage, 8)
#         b.in[msg.SessionKey] = ch
#         go b.sessionLoop(msg.SessionKey, ch)   # one goroutine OWNS this chan
#     }
#     b.mu.Unlock()
#     ch <- msg
#
# The "lock" semantics fall out of structure: only one goroutine reads
# from ch, so two messages on the same key necessarily process serially.
# Different keys = different goroutines = parallel.


# ────────────────────────────────────────────────────────────────────
# What this excerpt teaches (vs. learn-nanobot/s05)
# ────────────────────────────────────────────────────────────────────
#
# 1. Session is a dumb data bag. Both languages.
#
# 2. SessionManager owns the cache + (in upstream) disk I/O. learn-nanobot
#    s05 ports only the cache; s06 ports the disk part.
#
# 3. MessageBus is two queues. Upstream is intentionally tiny here —
#    its only job is to detach channels from the agent. The actual
#    serialization is in AgentLoop. learn-nanobot fuses them because
#    Go's chan + goroutine is more naturally one primitive.
#
# 4. Per-session lock is implicit in Go. Where Python writes
#    `async with self._session_locks.setdefault(key, asyncio.Lock())`,
#    Go writes "create a chan and dedicate one goroutine to it" — the
#    Go runtime's scheduler does the rest.
#
# 5. learn-nanobot's Bus also handles `Stop()` drain; upstream's Bus
#    delegates this to the AgentLoop's shutdown sequence in
#    nanobot/agent/loop.py::stop. Same idea, different layer.
#
# Reading map:
#   - nanobot/session/manager.py:1-100  — Session + get_or_create + load
#   - nanobot/session/manager.py:200-400 — save + flush_all + delete
#   - nanobot/bus/queue.py              — the entire MessageBus (~50 LOC)
#   - nanobot/bus/events.py             — InboundMessage + OutboundMessage
#   - nanobot/agent/loop.py             — _session_locks + _process_inbound
#                                        (canonical for learn-nanobot/s11)
#   - nanobot/channels/telegram.py      — a real channel adapter that
#                                        creates InboundMessages
