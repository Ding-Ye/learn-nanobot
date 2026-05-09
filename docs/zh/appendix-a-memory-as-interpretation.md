---
title: "附录 A · 记忆是诠释，不是转录"
chapter: A
slug: appendix-a-memory-as-interpretation
est_read_min: 18
---

# 附录 A · 记忆是诠释，不是转录

> 这是 nanobot 最有意思、也最容易被误读的设计。一句话讲：**agent 的记忆有两层；上面一层不是 log，是 LLM 写的 README**。

---

## 看到的是文件，看不到的是设计

学完 s06 你知道 nanobot 的工作区里有四个文件：

```
~/.learn-nanobot/agents/default/
├── history.jsonl    ← 1 行 1 个 turn，append-only，永远不删
├── MEMORY.md        ← 关于"事实"的 markdown
├── SOUL.md          ← 关于"agent 自己是谁"的 markdown
└── USER.md          ← 关于"用户偏好"的 markdown
```

如果只看名字，会以为这是个老套的"日志 + 三个 setting 文件"。但是：

1. **`history.jsonl` 是机器写的，三个 `.md` 是 LLM 写的**——后者不是用户编辑的，是 agent 自己周期性"reflect"出来的。
2. **三个 markdown 文件可以被 LLM 改写**——这正是上游 nanobot 的 *Dream* 阶段做的事：agent 读自己的 history，调 `edit_file` 工具改写 `MEMORY.md`。
3. **是 *interpretation* 不是 *transcription***——`history.jsonl` 是事实记录，三个 markdown 是 *agent 对自己经历的理解*。

## 三层心智模型

```
                    HISTORY                        MEMORY
                  (转录层)                        (诠释层)
                                  ┌──────────►
                                  │
            history.jsonl ────────┤              MEMORY.md
            (append-only,         │              (事实与观察)
             永远不丢)             │
                                  ├──────────►
                                  │              SOUL.md
            ↑ Consolidator (s10)  │              (agent 人格)
            ↑   折叠到一段总结    │
                                  │              USER.md
            ↑ Dream (上游有,我们没做)             (用户偏好)
            ↑ 周期性读 history,
            ↑ 调 edit_file 改三个 .md
            ↑
```

第一层：**append-only history**。它是 agent 经历的客观转录。永远不删，不改。这是 audit trail。

第二层：**curated markdown**。它是 LLM 写给未来自己看的 README。可以被改写——因为新经验会让旧理解过时。这层是 *interpretation*。

中间是两个 LLM 操作把它们连起来：

- **Consolidator**（你在 s10 写过了）—— history 太长时折叠最早的一段成 summary，但 *summary 还是事实*，只是更短。
- **Dream**（上游 nanobot 有，我们 learn-nanobot 没做）—— 周期性读 history，让 LLM 调 `edit_file` 工具去 *改写* `MEMORY.md` / `SOUL.md` / `USER.md`，相当于"重新理解"自己经历的事。

## 为什么这套设计赢过两个 obvious alternatives

### 替代方案 1：纯 append-only log

把所有事都 append 到一个无限长的 log 里。Pro：永不丢信息。Con：context window 会爆——agent 每次都要载入全部历史。Solution 表面上是 RAG（向量检索），但 agent 的记忆不全是"找信息"，更多是"知道自己是谁"。

### 替代方案 2：纯 in-place markdown

把所有事都直接 sync 到 `MEMORY.md` 一个文件里，需要时载入。Pro：context 紧凑。Con：丢失 audit trail，模型一旦改错就没法恢复；并且"事实"和"理解"混在一起，分不清"刚发生什么"和"agent 觉得什么"。

### nanobot 的选法

**两层都保留**。底层不可变；顶层可变。中间用 LLM 做压缩与改写。这样：

- audit trail 在 history.jsonl
- 参与 prompt 的是 markdown（紧凑）
- agent 可以"修正自己"——而不是被 immutable history 永远绑定

这是工程意义上的 *记忆*，不是数据库意义上的存储。

## 我们为什么没做 Dream

learn-nanobot 教到 Consolidator 就停了，因为：

1. Consolidator 已经把"两层 + LLM 压缩"的模式完整展示了
2. Dream 在工程上是 Consolidator 的更激进版（修改 markdown 而非追加 summary），心智模型不变
3. 从代码量来说 Dream 大约 300 LOC，单独成章不够，加进 s10 又会膨胀

App. B 把 Dream 列为"延伸练习 #3"——你做完 learn-nanobot 后，应该能仅凭这一节的设计描述，独立把 Dream 写出来。

## 跨章节交叉引用

- **s06**：四文件的物理布局
- **s10**：Consolidator 的实现，第一层压缩
- **s_full** 第 16 步：AutoCompact 在后台 wake 这个流程
- **App. B**：上游 `nanobot/agent/memory.py` 的 Dream 类是这一节描述的实物

## 一句话带走

agent 是 stateful 的，跨 turn、跨 session 都是。但这种 stateful 是 *non-monotonic* 的——agent 被允许 *修正* 自己关于过去的理解。这是它和 chatbot 的根本区别。
