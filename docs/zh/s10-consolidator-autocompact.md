---
title: "s10 · Consolidator 与 AutoCompact"
chapter: 10
slug: s10-consolidator-autocompact
est_read_min: 28
---

# s10 · Consolidator 与 AutoCompact

> 教什么：file-backed history 是会无限增长的。跑几十轮之后 prompt 撑爆模型的 context window，延迟飙升、cost 飙升、最后请求直接失败。上游的答案是个两件套：`Consolidator` 在 `Session.Messages` 里挑一个**合法 user-turn 边界**，让 LLM 把那段消息压成一段话，写到 `<sessionKey>-summary.md`，并把 `Session.LastConsolidated` 推过这个边界。`AutoCompact` 是个 TTL ticker，扫闲置 session 在后台跑 consolidator——这样下次用户回来时 prompt 已经被压小了。本节有三个教学点：（1）你不能在任意位置切——协议合法性是硬约束；（2）token 估算是近似的，这是有意为之；（3）AutoCompact 是 fire-and-forget 而且幂等，不能跟 agent loop 抢同一个 session。

---

## Problem / 问题

到 s09 为止 agent loop 形态上已经齐全：provider 抽象、tool registry、runner、session、bus、memory、skills、context、hooks。但有个运维大坑：每一轮都往 `Session.Messages` 追加，每次构 system prompt 都读完整的 un-consolidated tail。CLI 跑 50 轮"总结这个 PDF""搜那个事实"，你会眼睁睁看着 system prompt 从 600 字符长到 60 KB。token 数撑过预算，模型越来越频繁返回 `finish_reason="length"`，length-recovery 路径被触发，最后下一个请求直接 400 报"request exceeds context window"。

你不能傻傻地砍 tail 解决问题。Anthropic 协议——以及 OpenAI 在 tool-use 模式下——对消息顺序有结构性约束。`tool_use` 块后面必须紧跟匹配 `tool_use_id` 的 `tool_result`；`assistant` 消息不能是 LLM 看到的最后一条（如果还要继续说话）；`user` 角色的 tool_result 回复属于它前面的 tool_use 调用，硬把它们拆开就把调用悬挂了。所以你需要**结构上合法的切点**：消息流能在那里合理暂停又合理恢复，LLM 看不出来。

上游的答案是 `nanobot/agent/memory.py:880` 的 `Consolidator.pick_consolidation_boundary`。从 `Session.LastConsolidated` 开始向后扫 un-consolidated tail，累加每条消息的 token 估算，返回最后一个满足以下两个条件的 user-message 索引：（a）累加移除的 tokens 达到目标阈值；（b）这条消息是个新鲜 user prompt（不是 tool_result 回复）。如果不存在这种索引，返回 None；调用方退化到 "raw archive" 模式（不摘要直接 dump）——s10 不打这个 fallback，我们用 `ErrNoLegalBoundary` 暴露给调用方让它决定。

拿到边界后剩下的就是机械活：把 `messages[start:end]` 渲染成文本、用摘要 system prompt 调 `Provider.Chat`、把结果写到 `<sessionKey>-summary.md`、推进 `Session.LastConsolidated = end`。后续 turn 从 `LastConsolidated` 往后切，被摘要那一段就从 prompt 消失了。摘要本身可以再注回下一轮的 system prompt（App. A "memory as interpretation" 模式），也可以留作磁盘审计 artifact。

没人警告你的那个坑：在 turn **当中**同步做这件事，等于给本来就超预算的 turn 多加一轮 LLM round-trip。所以上游把它甩到一个后台 ticker——`AutoCompact`——只对闲置 session 跑。下次用户回来时 prompt 已经被压成一半，前台 turn 不背摘要的成本。代价是：summarizer 可能跟 bus 抢同一个 session 的 `Session.Messages`——如果 bus 在摘要进行中又拿起这个 session，所以 AutoCompact 必须 (1) 跳过当前 active 的 session，(2) 跳过已经在摘要中的 session。两条不变量都靠按 session key 索引的 `archiving` map 守住。

## Solution / 解决方案

`consolidator.go` 里 3 个新类型：

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

外加主力方法：

```go
func NewConsolidator(p Provider, mem *MemoryStore) *Consolidator
func (c *Consolidator) PickBoundary(s *Session, tokensToRemove int) (Boundary, bool)
func (c *Consolidator) Consolidate(ctx context.Context, s *Session) (string, error)
```

`autocompact.go` 再加两个：

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

`MemoryStore` 多一个方法（s09→s10 唯一的 carry-over 改动）：

```go
func (m *MemoryStore) WriteSummary(sessionKey, summary string) error
func (m *MemoryStore) ReadSummary(sessionKey string) (string, error)  // 顺手提供 round-trip
```

摘要落到 `<root>/<sessionKey>-summary.md`。session key 会先 sanitize（`:` `/` 空格变 `_`）保证任何文件系统上都安全。原子性靠 `tmp + os.Rename`，跟 `WriteCursor` 同一套。

`Session.LastConsolidated` 在 s06 是个占位字段——这一节才真正有人读写。摘要前游标是 `s.LastConsolidated`；`Consolidate` 成功返回后游标是 `boundary.End`。后续 history slice 用这个把已摘要的 tail 砍掉。

## How It Works / 工作原理

```ascii-anim frames=4
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  Session.Messages                                                │
│    [u, a, u, a, u, a, u, a, u, ...]                              │
│         ^                                                        │
│         LastConsolidated = 0                                     │
│                                                                  │
│  Consolidator.PickBoundary(s, tokensToRemove=N):                 │
│    扫 idx = 0..len(Messages)                                     │
│    if idx > start && Role=="user" && !isToolResult(msg) &&       │
│       removed >= N:                                              │
│         last = Boundary{Start: 0, End: idx}                      │
│    返回最后那个候选                                                │
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
│        if activeKeys[key] { continue }      // 神圣：在 flight    │
│        if a.archiving[key] { continue }     // 已在跑了          │
│        if now - sess.UpdatedAt <= TTL { continue }  // 没闲       │
│        a.archiving[key] = true              // 起 goroutine 之前 │
│        go a.runOne(ctx, sess)               // fire-and-forget   │
│                                                                  │
│  AutoCompact.runOne(ctx, sess):                                  │
│    defer { delete(a.archiving, sess.Key) }                       │
│    summary, _ := Consolidator.Consolidate(ctx, sess)             │
│    a.summaries[sess.Key] = summary                               │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

核心约 30 行（节选自 [`agents/s10-consolidator-autocompact/consolidator.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s10-consolidator-autocompact/consolidator.go)）：

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

**5 个非显然之处**：

1. **边界遍历挑的是最后一个候选，不是第一个**。一旦 `removed >= tokensToRemove`，后续每个 user message 都是合法 stop 点。挑最后一个是因为这样最大化本轮被砍量、最小化重跑次数。上游做法一样（`last_boundary` 每命中一次就被覆盖、不会提前返回）。

2. **`isToolResultMessage` 是结构判定，不是启发式**。user 角色的消息如果它的所有 content block 都是 `tool_result` 类型，那就是个 tool 回复，不是新鲜 user prompt。在 `assistant`（带 `tool_use`）和对应 `tool_result` 之间切，会把那次调用悬挂掉。判定方法是直接遍 content blocks——没有字符串匹配、没有 role 猜测。

3. **Token 估算是近似的**。我们 ship `len(s)/4` 作为默认 `TokenEst`，因为对英文文本它在 2x 误差以内、而且非常好读。Production 里 swap 成 `pkoukk/tiktoken-go`（一行：`c.TokenEst = realTiktoken`）。边界 picker 是保守的——只在 removed >= target 才停，所以即使估算偏 2x，最坏情况也只是"砍多了一倍"，不影响正确性。

4. **摘要存为单独文件**，不是原地编辑 `history.jsonl`。`history.jsonl` 是审计层（append-only、永不修改）；摘要是诠释层（LLM 生成、可以重做）。把它们放在不同文件保留了审计——App. A "memory as interpretation" 模式就立在这条上。

5. **AutoCompact 永远不阻塞 agent loop**。`CheckExpired` 返回它排队的 session keys 列表，goroutine 在后台跑。`archiving` map 是幂等护栏——第一个 goroutine 还没跑完时第二次 `CheckExpired` 不会重复排队。神圣 active-keys 过滤防住 bus 跟 consolidator 抢 `Session.Messages`。

## What Changed / 与上一节的变化

```diff
  // s09/memory.go：MemoryStore 有 AppendHistory + ReadCursor + WriteCursor +
  //                 ReadMemory + WriteMemory。没有摘要持久化
- type MemoryStore struct { root string }
- // (没有 WriteSummary)

  // s10/memory.go：MemoryStore 加 WriteSummary + ReadSummary，其余原样
+ func (m *MemoryStore) WriteSummary(sessionKey, summary string) error {
+     target := filepath.Join(m.root, summaryFilename(sessionKey))
+     return atomicWrite(target, []byte(summary))
+ }
+ func (m *MemoryStore) ReadSummary(sessionKey string) (string, error) { ... }
+ func summaryFilename(sessionKey string) string {
+     return strings.NewReplacer(":", "_", "/", "_", " ", "_").
+         Replace(sessionKey) + "-summary.md"
+ }

  // s09：Session.LastConsolidated 是占位字段，没人读没人写
- // s10：Consolidator.Consolidate 把 s.LastConsolidated 推到 boundary.End
+ s.LastConsolidated = boundary.End
```

新增类型（s10 的核心）：

```go
+ type Boundary struct { Start, End int }
+ type Consolidator struct { Provider Provider; TokenEst func(string) int;
+                            ConsolidationRatio float64; Memory *MemoryStore; Model string }
+ type AutoCompact struct { Sessions *SessionManager; Consolidator *Consolidator;
+                           TTL time.Duration; archiving map[string]bool;
+                           summaries map[string]string }
+ var ErrNoLegalBoundary = errors.New(...)
```

`Provider` / `Tool` / `Registry` / `Bus` / `SessionManager` / `Hook` / `ContextBuilder` / `SkillLoader` / `Runner` 全部一行没改。`main.go` 加了个 `-autocompact-ttl` flag、起了一个 `autocompact.Run` 的 goroutine。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s10-consolidator-autocompact

# 单 turn 行为跟 s09 一样；新的 -autocompact-ttl flag 接好了但
# 单 turn 不会触发（没经过闲置窗口）
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

跑测试（race detector 必开——AutoCompact 起 goroutine）：

```bash
go test -count=1 -race -v ./...
```

期望 9 个测试全过：

- `TestConsolidator_PickBoundary_LegalUserTurnOnly` —— 跨过 assistant 找到真 user turn；
- `TestConsolidator_NoLegalBoundary_ReturnsFalse` —— 短 session + 大 `tokensToRemove` 返 `ok=false` 与 `ErrNoLegalBoundary`；
- `TestConsolidator_Consolidate_PersistsSummary` —— `<key>-summary.md` 写入 LLM 的回复；
- `TestConsolidator_AdvancesLastConsolidated` —— `s.LastConsolidated` 推到 `boundary.End`；
- `TestConsolidator_PickBoundary_SkipsToolResultMessages` —— 加分项：纯 tool_result 的 user 消息不是合法切点；
- `TestAutoCompact_IdleSession_GetsCompacted` —— TTL=50ms 的 happy path；
- `TestAutoCompact_ActiveSession_Skipped` —— `activeKeys` 中的 session 被过滤；
- `TestAutoCompact_AlreadyArchiving_Skipped` —— 连续两次 `CheckExpired` 不会重复排队；
- `TestAutoCompact_RunLoop_StopsOnContextCancel` —— Run 在 ctx cancel 时干净退出。

要不调真 LLM 看 consolidator 端到端，跑 verbose 单测，盯测试 temp 目录里的 `cli_idle-summary.md` 文件。

## Upstream Source Reading / 上游源码阅读

s10 对上游两个文件：`nanobot/agent/memory.py:800-1200`（`Consolidator` 类加 `pick_consolidation_boundary` 与 `archive`）和 `nanobot/agent/autocompact.py`（`AutoCompact` 类）。两份合起来约 550 行 Python；翻 200 行核心到 Go。

完整注解版在 [`upstream-readings/s10-consolidator-autocompact.py`](../../upstream-readings/s10-consolidator-autocompact.py)。

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

**5 条对照阅读要点**：

- **上游即使没满足 token 阈值，也会返回 `last_boundary`**。我们 Go 版没满足时返 `(Boundary{}, false)`。Python 那边在 removed_tokens < target 时仍然返回最后一个 user-message 元组，由调用方检查元组的第二个元素决定要不要用。效果等价，我们让 boolean 显式是因为 Go 不像 Python 那样靠 truthiness 灵活处理元组。

- **上游用 `tiktoken.get_encoding("cl100k_base")` 做真实 token 计数**。我们默认 `len(s)/4`。trade-off 写在 `TokenEst` 字段注释里：production swap 成 `pkoukk/tiktoken-go`。边界 picker 故意保守，所以 2x 偏差的估算最多就是"砍掉的比需要的多两倍"，不破坏正确性，只是过度压缩。

- **上游 AutoCompact 有个 `_summaries` 缓存**——跨 `check_expired` 调用还在，下次 `prepare_session` 把最新摘要注回 runtime context。我们用 `summaries map[string]string` 镜像，并暴露 `LastSummary(key)` 让 `main.go` 可以打印。完整的注回流（`prepare_session` 读缓存、把"Inactive for N minutes..."拼到前面）落在 s11，不是 s10。

- **上游 archive 在 LLM 调用失败时退化到 `raw_archive`**。raw_archive 不摘要直接 dump 到 history.jsonl——是个面包屑，保住数据。s10 不打这个 fallback；测试面小、fallback 主要是防御性的、`ErrNoLegalBoundary` 已经能传达"压不动了，让它放着"。如果你拿 learn-nanobot 上 production 用，加上。

- **上游的 `_RECENT_SUFFIX_MESSAGES = 8`** 和 `retain_recent_legal_suffix` 助手保最近 8 条不被归档。思路是会话进行中用户期望 agent 完美记得 immediate context——所以只压缩已经"老"的部分。我们的边界 picker 从 `LastConsolidated` 往后扫，对长尾 session 等价；但没强制"保最后 N 条"。如果你发现 agent 太激进地忘事，加个 `RetainSuffix int` 字段、跳过这个窗口内的消息就行。

**想读更多**：

1. `nanobot/agent/memory.py:_consolidate_replay_overflow` —— 回放窗口 consolidator。当 session 的 tail 超过 "replay max messages" 配置（与 token 预算不同），这个方法跑一次单 archive pass。s10 不打回放窗口 cap；token-based picker 对我们够用了。

2. `nanobot/agent/autocompact.py:_split_unconsolidated` —— 把 un-consolidated tail 拆成"要归档的"和"要保留在活动消息列表的"的助手。`_RECENT_SUFFIX_MESSAGES` 就在这里发挥。读这个能体会到生产版的 keep-last-N 不变量。

3. `nanobot/agent/memory.py:archive` 和 `consolidator_archive.md` 模板 —— 真正的摘要 prompt。我们 `summarizationSystem` 这个 const 抓了意图（保 facts、丢 scratchpad）但更短；上游模板约 80 行，专门为 re-injection 进下一轮 system prompt 调过。

---

**下一节预告**：s11 —— 把 11 节所有积木拼进 TurnState 状态机。

Next: s11 — composing everything into the TurnState state machine.
