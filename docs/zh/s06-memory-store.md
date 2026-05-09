---
title: "s06 · MemoryStore（文件读写）"
chapter: 6
slug: s06-memory-store
est_read_min: 22
---

# s06 · MemoryStore（文件读写）

> 教什么：把 s05 留在内存里的 SessionManager 接到磁盘上。一个文件 append-only（`history.jsonl` —— 真相），三个文件 atomic-replace（`MEMORY.md` / `SOUL.md` / `USER.md` —— 诠释），再加一个独立的 `.cursor` 让 s10 的 Consolidator 知道"读到哪儿了"。

---

## Problem / 问题

s05 的 SessionManager 只活在内存里。进程一退出，所有对话历史就都没了。一个真正的 agent 要做到：

- **跨进程持久化**：今天 CLI 跑一次，明天接着跑，agent 还记得我叫什么。
- **跨重启不丢真相**：哪怕 LLM 最后一轮回复说错了，原始记录得在；只有这样后来的 Dream 才能"重读"并修正。
- **可被高层覆写**：诸如"用户偏好简体中文"、"我是 Sam，住在 SF"这类**长期事实**，不能是流水账，得有一份**诠释过的、可替换的**精简版本——这才进得了系统提示。

上游 nanobot 的解法是经典 Unix-y：**所有状态都是文件**。`nanobot/agent/memory.py` 里的 `MemoryStore` 维护四份独立文件，每份都有自己明确的写入语义。

## Solution / 解决方案

新增一个文件 `memory.go`，引入两个类型 + 一个常量：

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
    root     string       // <root>/ 是一个 agent 的全部状态
    appendMu sync.Mutex   // O_APPEND 的 belt-and-braces
}

var BootstrapFiles = []string{"AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md"}
//                               s08 的 ContextBuilder 会读这四份

func NewMemoryStore(root string) (*MemoryStore, error)               // mkdir -p
func (m *MemoryStore) AppendHistory(e HistoryEntry) (cursor int, err) // append-only
func (m *MemoryStore) ReadCursor() (int, error)                      // 缺省 0
func (m *MemoryStore) WriteCursor(c int) error                       // tmp + rename
func (m *MemoryStore) ReadMemory(filename string) (string, error)     // missing 返回 ""
func (m *MemoryStore) WriteMemory(filename, body string) error        // tmp + rename
```

四个文件、两种写入语义：

| 文件 | 写入语义 | 角色 | 谁写 |
|---|---|---|---|
| `history.jsonl` | append-only，每行一个 `HistoryEntry` | 真相、审计、回放 | s06 的 Bus（每次 turn 后） |
| `.cursor` | atomic replace（tmp + os.Rename） | "我成功记到第 N 行了" | AppendHistory 内部、s10 |
| `MEMORY.md` | atomic replace | LLM 的长期事实诠释 | s10（Consolidator）/ App. A（Dream） |
| `SOUL.md` | atomic replace | agent 的人格 | App. A（Dream） |
| `USER.md` | atomic replace | 用户偏好画像 | App. A（Dream） |

`Session.LastConsolidated` 这个字段在 s05 是 placeholder，从 s06 起**真正被写**：每次 `AppendHistory` 成功后，新的 cursor 落到 `.cursor`，`Session.LastConsolidated` 在 s10 才被推进——它代表的是"摘要进度"，**不**是"日志长度"，所以两者必须分开。

## How It Works / 工作原理

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
│  AppendHistory 内部（O_APPEND + mutex + 写入 .cursor）：           │
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
│  │   history.jsonl  ──  append-only  ──  原始真相，永不丢失   │    │
│  │                                                            │    │
│  │   MEMORY.md      ──  atomic replace ──  LLM 的诠释，可改写 │    │
│  │   SOUL.md        ──  atomic replace ──  人格，App.A 的 Dream  │
│  │   USER.md        ──  atomic replace ──  用户画像，可改写   │    │
│  │   .cursor        ──  atomic replace ──  小整数，无需历史   │    │
│  │                                                            │    │
│  │  两种语义 = 两种角色：                                     │    │
│  │   "记下事实" → append；"修订诠释" → replace                │    │
│  └────────────────────────────────────────────────────────────┘    │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

核心约 25 行（节选自 [`agents/s06-memory-store/memory.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s06-memory-store/memory.go)）：

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

    cursor, err := m.countLines(historyPath)   // 重新数行（cheap, 文件按 PB 不会到）
    if err != nil { return 0, err }
    if err := m.writeCursorAtomic(cursor); err != nil { return 0, err }
    return cursor, nil
}

// atomic replace —— 同目录 tmp + os.Rename。POSIX 保证 rename 在同一文件系统内是原子的。
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

**4 个非显然之处**：

1. **`O_APPEND` + 互斥锁是 belt-and-braces**。POSIX 保证 `O_APPEND` 写小于 `PIPE_BUF`（典型 4 KB）的写是原子的——但我们的 JSON 行有可能超过这个限制，特别是 tool_result 内容。`appendMu` 兜住这个 gap；race detector 会确认没漏。
2. **`os.Rename` 是原子性的关键原语**。POSIX 规定**同一文件系统**内 rename 是原子的。我们 tmp 文件创建在**同一目录**（用 `os.CreateTemp(dir, ...)`），保证不会跨文件系统——这是关键细节，跨文件系统 rename 会退化成 copy+unlink，原子性就丢了。
3. **`.cursor` 不是 derived data，是独立 source-of-truth**。理论上 cursor = `wc -l history.jsonl`，但**两者解耦**才有意义：cursor 在 s10 里只在**摘要成功后**才推进，**不是**在每次 append 之后。"日志有多长" ≠ "我已经处理到哪里"——这俩在 s10 才会真的分叉。
4. **`ReadMemory` 缺失文件返回 `""`，不是 error**。这是给 s08 ContextBuilder 用的接口约定：第一次跑的 agent 没有 `MEMORY.md`，让上层不必专门 if-not-exists。错误（如权限问题）才返回 `error`。

## What Changed / 与上一节的变化

```diff
  // s05/main.go：跑完一轮就退出，所有 Session.Messages 进了垃圾回收
- bus.Send(InboundMessage{SessionKey: *sessionKey, UserText: prompt, ID: "cli-1"})
- out := <-bus.Out()
- fmt.Println(out.Text)
- bus.Stop()

  // s06/main.go：多了 -workspace 标志和 MemoryStore；turn 完成后落盘
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

新增类型（s06 的核心）：

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

`Bus.OutboundMessage` 多了一个字段：

```diff
  type OutboundMessage struct {
      SessionKey    string
      Text          string
      Err           error
      CorrelationID string
+     Result        AgentRunResult  // main 取 Usage / ToolsUsed 入库
  }
```

`Session.LastConsolidated` 字段从 s05 起就有，但**在 s06 之前从未被写**。从这一节起，它对应"上次 Consolidator 摘要到的 cursor"——s06 不写它（写它是 s10 的工作），但**这一节起这个语义首次成立**。

`Provider` / `Tool` / `Registry` / `Runner` / `Session` / `Bus` 的核心 API 没改一行。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s06-memory-store

# 跑一次，写到一个临时 workspace
go run . -workspace /tmp/agent-demo "list .go files in this directory"

# 看磁盘上的状态
ls -la /tmp/agent-demo
# 应该看到 .cursor + history.jsonl

cat /tmp/agent-demo/.cursor
# 2  —— 这一轮写了两个 entry（user + assistant）

head -1 /tmp/agent-demo/history.jsonl | python3 -m json.tool
# {"ts": "2026-05-09T...", "role": "user", "content": "list .go files..."}
```

跑测试（race detector 必开）：

```bash
go test -count=1 -race -v ./...
```

期望输出：

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

`Concurrent_ConsistentLines` 是这一节最有教学意义的测试：10 个 goroutine 并发各 append 一行，最终行数必须 = 10 且每行都是合法 JSON。如果你把 `appendMu.Lock()` 注释掉、并把 entry 内容加大到几十 KB，在 race detector 下会立刻看到 race report——这就是为什么我们说"O_APPEND 的原子性只到 PIPE_BUF"是个真实的边界，不是教科书理论。

如果你想体验"跨进程保留历史"：

```bash
# 第一次：写第一轮
go run . -workspace /tmp/demo "我叫小明"
cat /tmp/demo/.cursor   # 2

# 第二次：再写一轮，cursor 加到 4
go run . -workspace /tmp/demo "天气真好"
cat /tmp/demo/.cursor   # 4
wc -l /tmp/demo/history.jsonl  # 4 行
```

注意：当前的 SessionManager 还**不会从 history.jsonl 重建内存里的 Messages**——也就是说第二次跑时，LLM 看不到第一次的对话。这是**故意的**：s06 只教文件 I/O，s11 的 Loop 才负责 RESTORE 状态。

## Upstream Source Reading / 上游源码阅读

s06 对应 `nanobot/agent/memory.py:1-400`——文件 I/O 部分。Consolidator（行 800-1200）是 s10 的内容；Dream（更下面）是 App. A 的扩展练习。

完整注解版在 [`upstream-readings/s06-memory-store.py`](../../upstream-readings/s06-memory-store.py)。

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

**5 条对照阅读要点**：

- **上游有 `git` integration，我们没有**。`MemoryStore.git` 是一个 GitStore，把 SOUL/USER/MEMORY 自动 git commit 进 workspace 里——这是 nanobot 提供的"记忆变更可追溯"的真正杀手锏（每次 Dream 都是一次 commit，可以 `git log` 看到 agent 信念的演化）。我们 s06 不教这个：核心机制要先理解，再上 git。App. A 会讨论这个层级的扩展。
- **上游 `append_history` 的输入是字符串**，包含 `"cursor"` 字段；我们 Go 版输入 `HistoryEntry` 结构体。原因是 Go 的 `encoding/json` 默认更喜欢类型化结构。功能等价。
- **`_next_cursor` 上游有 fast path + slow path**：先信 `.cursor` 文件（快），不行就读最后一行的 cursor 字段（中），再不行扫全文取 max（慢）。我们简化成"`countLines(history.jsonl)`"——文件按 PB 算不会到，简单胜出。如果你的 agent 一天写百万 turn，再回来抄 fast path。
- **上游 `strip_think(raw)` 这一步我们没做**。它是把 `<think>...</think>` 里的 reasoning 块剥掉再持久化，避免下次 LLM 看见自己以前的"内心独白"被 echo 回来。这个逻辑会在 s08 的 ContextBuilder 里重做（anti-pattern #4：单一 sanitize 边界）。s06 只管原始 I/O。
- **上游有 legacy migration（`HISTORY.md` → `history.jsonl`）**。这是给老用户升级用的，我们 learn-nanobot 没有"老用户"，所以略过。读源码时遇到 `_maybe_migrate_legacy_history` 知道是历史包袱即可。

**想读更多**：

1. `nanobot/agent/memory.py:367-390` —— `_write_entries` 是 atomic write 的上游版（带 `os.fsync` + 目录 fsync）。我们 Go 版做了简化：只 fsync 文件不 fsync 目录，因为 macOS / Linux 上目录 fsync 在普通用户场景里不是必需的（且 Windows 上会失败）。
2. `nanobot/agent/memory.py:393-405` —— `.dream_cursor` 是第二个 cursor。Dream 推进它，Consolidator 推进 `.cursor`。两个 cursor = 两个独立的"读到哪儿"——s10 + App. A 都会用到。
3. `nanobot/utils/gitstore.py` —— GitStore 的实现。如果 Phase G 你想给 learn-nanobot 加 git 备份，这是参考。

---

**下一节预告**：s07 给 agent 加技能（Skills）。一个 `SkillsLoader` 走 workspace + builtin 目录、解析 YAML frontmatter、检查 `requires.bins` / `requires.env`、把可用的 skill 拼到 system prompt 里。`MemoryStore` 和 `Runner` 的 API 都不变。
