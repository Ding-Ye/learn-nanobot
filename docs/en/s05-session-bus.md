---
title: "s05 · Session + MessageBus"
chapter: 5
slug: s05-session-bus
est_read_min: 24
---

# s05 · Session + MessageBus

> What this teaches: wrap s04's Runner in a SessionManager + Bus. Each session gets its own goroutine + buffered chan. Turns within one session are serial; across sessions they run in parallel. This is the Go translation of "per-session asyncio.Lock".

---

## Problem

s04 gave us `Runner.Run(ctx, spec) → AgentRunResult`: a pure function that takes messages and returns messages. But real products handle **many concurrent conversations**: a CLI session, 50 Telegram users, 200 Slack channels. Each conversation owns its own history. Turns within one conversation must be serialized (you can't let the LLM see "question 1" and "question 2" interleave). Turns across conversations must run in parallel (one slow user can't block everyone else).

Upstream nanobot solves this in `nanobot/bus/queue.py` + `nanobot/session/manager.py`:

- **MessageBus** — two `asyncio.Queue`s, inbound (channel → agent) and outbound (agent → channel). Channels and the agent are decoupled.
- **SessionManager** — an in-memory `_cache: dict[str, Session]` plus one jsonl file per session on disk.
- **per-session `asyncio.Lock`** — two turns on the same key never run the LLM concurrently.

s05 ports this to Go. Go's concurrency primitives don't line up with Python's, so this chapter teaches the upstream mechanism *and* the Python→Go translation.

## Solution

Two new files:

```go
// session.go
type Session struct {
    Key              string
    Messages         []Message
    CreatedAt, UpdatedAt time.Time
    LastConsolidated int  // s10's Consolidator writes this
}

type SessionManager struct {
    mu       sync.RWMutex
    sessions map[string]*Session
}

func (sm *SessionManager) GetOrCreate(key string) *Session  // idempotent
func (sm *SessionManager) Get(key string) (*Session, bool)
func (s *Session) Append(msg Message)
func (s *Session) GetHistory(maxMsgs int) []Message

// bus.go
type Bus struct {
    sessions *SessionManager
    runner   *Runner
    tools    *Registry
    model    string
    maxIter  int
    mu       sync.Mutex
    in       map[string]chan InboundMessage  // one per session
    out      chan OutboundMessage             // shared
    wg       sync.WaitGroup
    done     chan struct{}
}

func (b *Bus) Send(msg InboundMessage)            // dispatch
func (b *Bus) Out() <-chan OutboundMessage         // read replies
func (b *Bus) Stop()                               // close + drain
```

Three key decisions:

1. **One buffered chan + one goroutine per session.** On the first Send for a new SessionKey the Bus creates a chan in `b.in[key]` and spawns `go b.sessionLoop(key, ch)`. This pair replaces Python's `asyncio.Lock`: because exactly one goroutine reads from each session's chan, turns are serialized by structure.
2. **The out chan is shared globally.** Every session goroutine writes to the same `b.out`. The reader (`main` / s11's channel adapter) sees replies from any session via one select.
3. **Stop is close + drain.** `Stop()` closes every inbound chan; the session goroutine's `for msg := range ch` drains the buffered messages and exits cleanly. `b.wg.Wait()` makes sure every goroutine actually finished.

## How It Works

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  main / s11 channel adapter                                      │
│  ─► bus.Send(InboundMessage{SessionKey: "tg:42", UserText: ...}) │
│                                                                  │
│  Bus.Send (briefly holds b.mu):                                  │
│    ch, ok := b.in[key]                                           │
│    if !ok {                                                      │
│      ch = make(chan InboundMessage, 8)                           │
│      b.in[key] = ch                                              │
│      go b.sessionLoop(key, ch)   ← first message: spawn goroutine│
│    }                                                             │
│    (unlock)                                                      │
│    ch <- msg                      ← push to this session's chan  │
│                                                                  │
│  sessionLoop("tg:42", ch):                                       │
│    for msg := range ch {            ← exits when chan is closed  │
│      sess := b.sessions.GetOrCreate("tg:42")                     │
│      hist := sess.GetHistory(120)                                │
│      result, _ := b.runner.Run(ctx, AgentRunSpec{                │
│          InitialMessages: append(hist, userMsg),                 │
│          Tools: b.tools, Model: b.model,                          │
│          MaxIterations: b.maxIter,                                │
│      })                                                          │
│      sess.Append(userMsg)                                         │
│      sess.AppendAll(result.Messages[len(initial):])               │
│      b.out <- OutboundMessage{Text: result.FinalContent, ...}     │
│    }                                                              │
│                                                                  │
│  ┌────────────────── Python ↔ Go translation ──────────────────┐ │
│  │                                                              │ │
│  │   asyncio.Lock per session   ↔  buffered chan + 1 goroutine  │ │
│  │   asyncio.gather             ↔  goroutine + sync.WaitGroup   │ │
│  │   asyncio.create_task        ↔  go func()                    │ │
│  │   asyncio.Queue              ↔  chan T (with capacity)       │ │
│  │   `async with lock:`         ↔  serialized by chan ownership │ │
│  │                                                              │ │
│  │  Python uses "explicit lock + many tasks" for exclusive use; │ │
│  │  Go uses "single goroutine owns the chan" — structurally     │ │
│  │  obvious without an extra Lock object.                       │ │
│  └──────────────────────────────────────────────────────────────┘ │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

The core ~30 lines (excerpt from [`agents/s05-session-bus/bus.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s05-session-bus/bus.go)):

```go
func (b *Bus) Send(msg InboundMessage) {
    b.mu.Lock()
    select {
    case <-b.done:
        b.mu.Unlock()
        // bus already stopped — emit an error outbound so the caller
        // doesn't block forever waiting for a reply.
        go func() { b.out <- OutboundMessage{SessionKey: msg.SessionKey,
            Err: fmt.Errorf("bus: stopped, dropping %q", msg.ID),
            CorrelationID: msg.ID} }()
        return
    default:
    }
    ch, ok := b.in[msg.SessionKey]
    if !ok {
        ch = make(chan InboundMessage, 8)
        b.in[msg.SessionKey] = ch
        b.wg.Add(1)
        go b.sessionLoop(msg.SessionKey, ch)
    }
    b.mu.Unlock()       // unlock BEFORE send — otherwise a slow consumer
                         // blocks dispatch for every other session.
    ch <- msg
}

func (b *Bus) sessionLoop(key string, ch <-chan InboundMessage) {
    defer b.wg.Done()
    ctx := context.Background()
    for msg := range ch {                     // exits when chan is closed
        sess := b.sessions.GetOrCreate(key)
        history := sess.GetHistory(0)
        userMsg := Message{Role: "user", Content: []ContentBlock{{Type: "text", Text: msg.UserText}}}
        initial := append(history, userMsg)
        result, err := b.runner.Run(ctx, AgentRunSpec{
            InitialMessages: initial, Tools: b.tools,
            Model: b.model, MaxIterations: b.maxIter,
        })
        sess.Append(userMsg)
        if len(result.Messages) > len(initial) {
            sess.AppendAll(result.Messages[len(initial):])
        }
        b.out <- OutboundMessage{SessionKey: key, Text: result.FinalContent,
            Err: err, CorrelationID: msg.ID}
    }
}
```

**4 non-obvious points**:

1. **`Bus.in` is `map[key]chan` plus a mutex.** A chan is goroutine-safe by spec; the *map* writes are not. `b.mu` only guards map mutations — once you have the chan pointer, sending on the chan does not need to hold the lock.
2. **The send happens after unlock.** If `ch <- msg` ran while holding `b.mu`, a slow consumer in session A would block dispatch for every other session — *and* would block `Stop()`'s `mu.Lock()`. This is the textbook "don't hold a global lock during a potentially blocking call" trap.
3. **`SessionManager.GetOrCreate` is double-checked.** Fast path: RLock + cache check. Miss: upgrade to Lock, then re-check before creating — otherwise two goroutines could both create a *Session for the same key. Standard double-checked locking.
4. **Drain on Stop is `for range ch`'s natural behavior.** `Stop()` closes one chan; the session loop's `for msg := range ch` processes the buffered messages and then sees the close and exits. We don't write a single line of "drain" code — the chan semantics make it free.

## What Changed (vs. s04)

```diff
  // s04: main calls the Runner directly
- runner := NewRunner(provider)
- result, err := runner.Run(ctx, AgentRunSpec{...})
- fmt.Println(result.FinalContent)

  // s05: main goes through the Bus
+ runner := NewRunner(provider)
+ sm := NewSessionManager()
+ bus := NewBus(sm, runner, registry, *model, *maxTurns)
+ bus.Send(InboundMessage{SessionKey: *sessionKey, UserText: prompt, ID: "cli-1"})
+ out := <-bus.Out()
+ if out.Err != nil { log.Fatalf(...) }
+ fmt.Println(out.Text)
+ bus.Stop()
```

New types (the s05 spine):

```go
+ type Session struct {
+     Key              string
+     Messages         []Message
+     CreatedAt, UpdatedAt time.Time
+     LastConsolidated int
+ }
+ type SessionManager struct {
+     mu       sync.RWMutex
+     sessions map[string]*Session
+ }
+ type Bus struct {
+     sessions *SessionManager
+     runner   *Runner
+     tools    *Registry
+     model    string
+     maxIter  int
+     mu       sync.Mutex
+     in       map[string]chan InboundMessage
+     out      chan OutboundMessage
+     wg       sync.WaitGroup
+     done     chan struct{}
+ }
+ type InboundMessage struct { SessionKey, UserText, ID string }
+ type OutboundMessage struct { SessionKey, Text, CorrelationID string; Err error }
```

`Runner` is unchanged — not one line touched. s05 is composition, not a refactor.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s05-session-bus

# One-shot CLI (default -session cli:direct)
go run . "list .go files in this directory"

# Tests REQUIRE the race detector — this is the first chapter with real
# goroutine concurrency.
go test -count=1 -race -v ./...
```

Expected test output:

```
=== RUN   TestBus_TwoSessions_Parallel
--- PASS: TestBus_TwoSessions_Parallel (0.10s)
=== RUN   TestBus_SameSession_Serial
--- PASS: TestBus_SameSession_Serial (0.10s)
=== RUN   TestBus_StopBeforeProcessing_Drains
--- PASS: TestBus_StopBeforeProcessing_Drains (0.07s)
=== RUN   TestSession_AppendMessage_OrderPreserved
=== RUN   TestSessionManager_GetOrCreate_Idempotent
=== RUN   TestSession_GetHistory_RespectsMaxMsgs
PASS
```

`TwoSessions_Parallel` is the most pedagogically loaded test: each FakeProvider sleeps 100ms; wall-clock must finish in <180ms. A serial implementation would clock ~200ms+ and fail.

To watch "same-session multi-turn keeps history" by hand:

```go
bus.Send(InboundMessage{SessionKey: "demo", UserText: "my name is Sam", ID: "1"})
out1 := <-bus.Out()
bus.Send(InboundMessage{SessionKey: "demo", UserText: "what's my name?", ID: "2"})
out2 := <-bus.Out()  // LLM should answer "Sam" — history was preserved
```

You can't see this across two `go run` invocations, because SessionManager is still in-memory only. s06's MemoryStore puts it on disk.

## Upstream Source Reading

s05 maps to two upstream files:

- `nanobot/session/manager.py` (~400 LOC; we focus on Session + get_or_create)
- `nanobot/bus/queue.py` (~50 LOC) plus `nanobot/bus/events.py` (~40 LOC)

Full annotated excerpt at [`upstream-readings/s05-session-bus.py`](../../upstream-readings/s05-session-bus.py).

```upstream:nanobot/session/manager.py#L42-L80
@dataclass
class Session:
    """A conversation session."""

    key: str  # channel:chat_id
    messages: list[dict[str, Any]] = field(default_factory=list)
    created_at: datetime = field(default_factory=datetime.now)
    updated_at: datetime = field(default_factory=datetime.now)
    metadata: dict[str, Any] = field(default_factory=dict)
    last_consolidated: int = 0  # Number of messages already consolidated to files

    def add_message(self, role: str, content: str, **kwargs: Any) -> None:
        """Add a message to the session."""
        msg = {
            "role": role,
            "content": content,
            "timestamp": datetime.now().isoformat(),
            **kwargs
        }
        self.messages.append(msg)
        self.updated_at = datetime.now()
```

```upstream:nanobot/bus/queue.py#L1-L30
class MessageBus:
    """
    Async message bus that decouples chat channels from the agent core.
    """

    def __init__(self):
        self.inbound: asyncio.Queue[InboundMessage] = asyncio.Queue()
        self.outbound: asyncio.Queue[OutboundMessage] = asyncio.Queue()

    async def publish_inbound(self, msg: InboundMessage) -> None:
        await self.inbound.put(msg)

    async def consume_inbound(self) -> InboundMessage:
        return await self.inbound.get()
```

**5 reading notes**:

- **Upstream's MessageBus only has two queues.** It does **not** carry per-session locks or per-session queues. Per-session serialization lives in `nanobot/agent/loop.py` as `_session_locks: dict[str, asyncio.Lock]`. We fuse the two ideas into one Go `Bus` because Go's "chan + goroutine" pair naturally combines a queue and a lock: a chan with one dedicated reader is *both*.
- **Upstream's SessionManager has disk I/O; ours doesn't.** `_load`, `save`, `_repair`, `flush_all`, `migrate` are all file-related. They're s06's responsibility. We only do the in-memory `_cache` here.
- **Upstream's Session.add_message stamps timestamps; we don't.** Upstream stores `timestamp: datetime.now().isoformat()` on every message and uses `get_history(include_timestamps=True)` to feed timestamps back to the LLM for relative-time reasoning. Our Message has no timestamp field — Phase G's hook system will introduce one if needed.
- **Upstream's get_history runs `_sanitize_assistant_replay_text`.** It strips `[Message Time: ...]` prefixes and `[image: ...]` breadcrumbs from assistant history so the LLM doesn't echo internal metadata back into its own replies. This is upstream anti-pattern #4 ("strip artifacts at exactly one boundary"). s08's ContextBuilder will own this in our curriculum.
- **Upstream locks the same key once.** `async with self._session_locks.setdefault(key, asyncio.Lock()):` — a typical Pythonism. Our Go implementation matches the semantics differently: only sessionLoop reads from `ch`, so the "lock-held region" *is* "the iteration of the for-range that consumed this msg". Read → process → next read = naturally serial.

**Read further**:

1. `nanobot/session/manager.py:200-400` — `_load` / `save` / `_repair`. s06 will rebuild this part.
2. `nanobot/agent/loop.py::_session_locks` — where upstream's per-session lock actually lives.
3. `nanobot/channels/telegram.py::handle_message` — a real channel adapter pushing InboundMessages onto the bus.

---

**Next**: s06 wires SessionManager to a MemoryStore so `Session.Messages` lands in `~/.nanobot/workspace/sessions/<key>.jsonl`, and curated facts land in `MEMORY.md`/`SOUL.md`/`USER.md`. The Bus and Runner shapes don't change.
