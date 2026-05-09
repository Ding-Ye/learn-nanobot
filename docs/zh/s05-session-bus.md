---
title: "s05 · Session 与 MessageBus"
chapter: 5
slug: s05-session-bus
est_read_min: 24
---

# s05 · Session 与 MessageBus

> 教什么：把 s04 的 Runner 包到 SessionManager + Bus 里——每个 session 一个 goroutine + buffered chan，同 session 内的 turn 串行、跨 session 并行。这是 Go 版本的"per-session asyncio.Lock"。

---

## Problem / 问题

s04 给我们一个 `Runner.Run(ctx, spec) → AgentRunResult` 的纯函数：吃 messages，吐 messages。但实际产品要面对**多个并行会话**：CLI 里有一条对话、Telegram 里有 50 个用户、Slack 里有 200 个 channel。每个会话有自己的历史，会话内的 turn 必须串行（不能让用户的"问题 1"和"问题 2"在 LLM 那边交错），跨会话却必须并行（一个慢用户不能堵住其他人）。

上游 nanobot 的解法在 `nanobot/bus/queue.py` + `nanobot/session/manager.py`：

- **MessageBus**：两个 `asyncio.Queue`，inbound（channel → agent）和 outbound（agent → channel），channel 和 agent 完全解耦。
- **SessionManager**：内存里 cache 每个 session（`_cache: dict[str, Session]`），磁盘上每个 session 一个 jsonl。
- **per-session `asyncio.Lock`**：同一 key 的两次 turn 不会并发跑 LLM。

我们的 s05 要把这套搬到 Go 里。Go 的并发原语和 Python 不一样，所以这一节教的同时是上游的机制 + Python→Go 的翻译。

## Solution / 解决方案

新增两个文件：

```go
// session.go
type Session struct {
    Key              string
    Messages         []Message
    CreatedAt, UpdatedAt time.Time
    LastConsolidated int      // s10 的 Consolidator 会写
}

type SessionManager struct {
    mu       sync.RWMutex
    sessions map[string]*Session
}

func (sm *SessionManager) GetOrCreate(key string) *Session  // 幂等
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
    in       map[string]chan InboundMessage  // 每个 session 一条
    out      chan OutboundMessage             // 全局共享
    wg       sync.WaitGroup
    done     chan struct{}
}

func (b *Bus) Send(msg InboundMessage)            // dispatch
func (b *Bus) Out() <-chan OutboundMessage         // 读 reply
func (b *Bus) Stop()                               // close + drain
```

三个关键决策：

1. **每个 session 一个 buffered chan + 一个 goroutine**。第一次 Send 一个新 SessionKey 时，Bus 在 `b.in[key]` 创建 chan，然后 `go b.sessionLoop(key, ch)`。这一对 chan + goroutine 替代了 Python 的 `asyncio.Lock`：因为只有一个 goroutine 在 read 这条 chan，turn 自然串行。
2. **out chan 是全局共享的**。所有 session goroutine 都往同一个 `b.out` 写。读端（`main` / s11 的 channel adapter）一个 select 就能拿到任意 session 的 reply。
3. **Stop 走 close + drain**。`Stop()` close 掉每条 inbound chan，session goroutine 在 `for msg := range ch` 里正常 drain 完队列里的剩余消息后退出。`b.wg.Wait()` 确保所有 goroutine 真的走完。

## How It Works / 工作原理

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  main / s11 channel adapter                                      │
│  ─► bus.Send(InboundMessage{SessionKey: "tg:42", UserText: ...}) │
│                                                                  │
│  Bus.Send (locks b.mu briefly):                                  │
│    ch, ok := b.in[key]                                           │
│    if !ok {                                                      │
│      ch = make(chan InboundMessage, 8)                           │
│      b.in[key] = ch                                              │
│      go b.sessionLoop(key, ch)   ← 第一次见到此 key, 起 goroutine │
│    }                                                             │
│    (unlock)                                                      │
│    ch <- msg                      ← 推到此 session 的 chan       │
│                                                                  │
│  sessionLoop("tg:42", ch):                                       │
│    for msg := range ch {            ← chan close 后 for 自然退出 │
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
│  ┌──────────────────── Python ↔ Go 翻译 ────────────────────────┐ │
│  │                                                              │ │
│  │   asyncio.Lock per session   ↔  buffered chan + 1 goroutine  │ │
│  │   asyncio.gather             ↔  goroutine + sync.WaitGroup   │ │
│  │   asyncio.create_task        ↔  go func()                    │ │
│  │   asyncio.Queue              ↔  chan T (with capacity)       │ │
│  │   `async with lock:`         ↔  自然由 chan 串行化           │ │
│  │                                                              │ │
│  │  Python 用"显式锁 + 多 task"实现独占；                         │ │
│  │  Go 用"单 goroutine 独占 chan"实现独占——结构更"一目了然"。       │ │
│  └──────────────────────────────────────────────────────────────┘ │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

核心 ~30 行（节选自 [`agents/s05-session-bus/bus.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s05-session-bus/bus.go)）：

```go
func (b *Bus) Send(msg InboundMessage) {
    b.mu.Lock()
    select {
    case <-b.done:
        b.mu.Unlock()
        // bus 已 stop，发个错误 outbound 让 caller 别死等
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
    b.mu.Unlock()       // 释放锁后再 send，否则慢消费者会卡住其他 session 的 dispatch
    ch <- msg
}

func (b *Bus) sessionLoop(key string, ch <-chan InboundMessage) {
    defer b.wg.Done()
    ctx := context.Background()
    for msg := range ch {                     // chan close 时自然退出
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

**4 个非显然之处**：

1. **`Bus.in` 是 `map[key]chan` 加 mutex**。chan 本身是 goroutine-safe 的（Go spec 保证），但 map 的写入不是。`b.mu` 只保护 map 写——一旦拿到 chan 指针，往 chan send 不需要持锁。
2. **send 必须发生在 unlock 之后**。如果 `ch <- msg` 在持有 `b.mu` 时执行，session A 的慢 consumer 会卡住所有其他 session 的 dispatch（连带 Stop 的 `mu.Lock()`）。这是 Go 并发里典型的"锁粒度别太大"陷阱。
3. **`sessions.GetOrCreate` 走 RWMutex 双检查**。fast path 用 RLock 看缓存命中；miss 时升级到 Lock + 再查一次（避免两个 goroutine 同时 Create 同一个 key）。这是教科书 double-checked-locking。
4. **Drain on Stop 是 `for range ch` 的天然行为**。`Stop()` close 一条 chan，sessionLoop 里的 `for msg := range ch` 会 process 完缓冲区里的剩余消息然后看到 close 并退出。我们不需要写一行 drain 代码——chan 的语义把这件事自动做了。

## What Changed / 与上一节的变化

```diff
  // s04: main 直接调 Runner
- runner := NewRunner(provider)
- result, err := runner.Run(ctx, AgentRunSpec{...})
- fmt.Println(result.FinalContent)

  // s05: main 通过 Bus 间接调
+ runner := NewRunner(provider)
+ sm := NewSessionManager()
+ bus := NewBus(sm, runner, registry, *model, *maxTurns)
+ bus.Send(InboundMessage{SessionKey: *sessionKey, UserText: prompt, ID: "cli-1"})
+ out := <-bus.Out()
+ if out.Err != nil { log.Fatalf(...) }
+ fmt.Println(out.Text)
+ bus.Stop()
```

新增类型（s05 的核心）：

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

`Runner` 没变，一行代码都没改——s05 是 composition，不是 refactor。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s05-session-bus

# 单次 CLI（默认 -session cli:direct）
go run . "list .go files in this directory"

# 跑测试 + 必须开 race detector，因为这一节有真正的 goroutine 并发
go test -count=1 -race -v ./...
```

期望测试输出：

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

`TwoSessions_Parallel` 是最有教学意义的：两个 FakeProvider 各自 sleep 100ms，wall-clock 必须 < 180ms。串行实现会跑成 ~200ms+ 直接挂掉。

如果你想手动构造"同 session 多 turn"的场景：

```go
bus.Send(InboundMessage{SessionKey: "demo", UserText: "我叫小明", ID: "1"})
out1 := <-bus.Out()
bus.Send(InboundMessage{SessionKey: "demo", UserText: "我叫什么", ID: "2"})
out2 := <-bus.Out()  // 此时 LLM 应该说"小明"——历史保留了
```

CLI 跑两次进程是看不到这个效果的，因为我们的 SessionManager 还是纯内存——s06 的 MemoryStore 才把它落到磁盘。

## Upstream Source Reading / 上游源码阅读

s05 对应两个上游文件：

- `nanobot/session/manager.py`（~400 LOC，我们对 Session + get_or_create 这两段）
- `nanobot/bus/queue.py`（~50 LOC）+ `nanobot/bus/events.py`（~40 LOC）

完整注解版在 [`upstream-readings/s05-session-bus.py`](../../upstream-readings/s05-session-bus.py)。

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

**5 条对照阅读要点**：

- **上游 MessageBus 只有两条 queue**——它**没有**per-session 的 lock 或 queue。per-session 串行是在 `nanobot/agent/loop.py` 里用 `_session_locks: dict[str, asyncio.Lock]` 实现的。我们把这两件事合在 `Bus` 里教，因为 Go 的"chan + goroutine"恰好把它们融合成一个原语：一条 chan 加一个独占 reader 同时实现了 queue + lock。
- **上游 SessionManager 有磁盘 I/O，我们没**。`SessionManager._load`、`save`、`_repair`、`flush_all` 都是文件相关；`migrate` 处理从老路径迁移。这些是 s06 的责任。我们 s05 只做内存里的 `_cache`。
- **Session.add_message 写时间戳，我们没**。上游每条消息存 `timestamp: datetime.now().isoformat()`，方便后续 `get_history(include_timestamps=True)` 回填给 LLM。我们 Message 里没这个字段——Phase G 加 Hook 后再补。
- **get_history 上游做了 sanitize_assistant_replay_text**。把 `[Message Time: ...]` 前缀和 `[image: ...]` 面包屑从 assistant 历史里剥掉，避免 LLM 模仿这种 metadata 在自己的回复里再次输出。这是上游 anti-pattern #4 的处理点。我们 s08 的 ContextBuilder 会引入 `Sanitize` 做这件事。
- **upstream 的 Lock 只 lock 一个 key 一次**。`async with self._session_locks.setdefault(key, asyncio.Lock()):`——典型 Python 习惯。我们 Go 用 chan 实现了同一语义：只有 sessionLoop 这一个 goroutine 在消费 ch，所以"持有锁的 region"就是"读到 msg 的那次 iteration"。读完 → process → 下一轮 read，自然串行。

**想读更多**：

1. `nanobot/session/manager.py:200-400` 看 `_load` / `save` / `_repair`——s06 会重做这部分。
2. `nanobot/agent/loop.py::_session_locks` 看上游 per-session lock 真正的实现位置。
3. `nanobot/channels/telegram.py::handle_message` 看一个真实 channel adapter 怎么往 Bus push InboundMessage。

---

**下一节预告**：s06 给 SessionManager 接上 MemoryStore，把 `Session.Messages` 落到 `~/.nanobot/workspace/sessions/<key>.jsonl`，把 facts 落到 `MEMORY.md`/`SOUL.md`/`USER.md`。Bus 和 Runner 都不变。
