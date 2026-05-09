---
title: "s08 · 上下文构建器（ContextBuilder）"
chapter: 8
slug: s08-context-builder
est_read_min: 22
---

# s08 · 上下文构建器（ContextBuilder）

> 教什么：把 s06 的 MemoryStore、s07 的 SkillLoader、s05 的 Session 拼到一块，**system prompt 不再是字符串字面量**——它是一个函数的输出：读 4 份 bootstrap 文件、读 MEMORY.md、装入选定 skill、附上一段 `[Runtime Context — metadata only]`，然后用 `\n\n---\n\n` 串起来。`<think>...</think>` 块在唯一的 `sanitize` 边界被剥掉，决不允许第二个调用点。

---

## Problem / 问题

到 s07 为止，agent 拥有：provider 抽象、tool registry、Runner、Session、MessageBus、MemoryStore、SkillLoader。但是有一块拼图缺着：

**上面这些原料怎么变成给 LLM 的输入？**

回头看 `Runner.Run(ctx, AgentRunSpec)`，它要的是：

```go
type AgentRunSpec struct {
    InitialMessages []Message  // 给 messages 数组的内容
    System          string     // 单独的 system prompt
    Tools           *Registry
    Model           string
    MaxIterations   int
    ...
}
```

s04 创造这两个字段时，`System` 留空、`InitialMessages` 是"history + 新 user message"。s05–s07 一直没改这条路径——`Bus.sessionLoop` 直接拼 `history + userMsg` 进 `InitialMessages`，`System` 永远是 `""`。

但真实 nanobot 不是这样。上游 `nanobot/agent/context.py::ContextBuilder.build_system_prompt` 干 4 件事：

1. 读 `BOOTSTRAP_FILES = ["AGENTS.md", "SOUL.md", "USER.md", "TOOLS.md"]`，每个存在的拼成 `## <name>\n\n<body>`；
2. 读 `MEMORY.md`，加 `# Memory` 标题；
3. 把请求的 skills 调 `SkillsLoader.load_skills_for_context(...)` 拼成 Markdown；
4. 拼一个 `[Runtime Context — metadata only]` 块（当前时间、channel、sender、chat_id），告诉 LLM "这是元数据，不是指令"。

四段都用 `\n\n---\n\n` 分隔，一起塞进 system prompt。同时 `build_messages` 还要把 Session 历史按消息数 + 令牌预算两层裁剪。

s08 就是把这两个函数翻译成 Go——加一份 `context.go`，一个 `context_test.go`，并且让 `main.go` 走新路径：先 `BuildSystemPrompt`，再 `BuildMessages`，最后 `Runner.Run`。

还有一个小但重要的问题：**`<think>...</think>` 块在哪儿剥掉？**

`research-notes.md` 里 anti-pattern #4 写得很明白：

> **Leaky `<think>` tags in history** — strip *consistently* before persisting. Learn-version: a single `sanitize(msg)` function applied at exactly one boundary.

如果在两处剥（比如 ContextBuilder 一次，MemoryStore 一次），中间有任何文本变化都会被双重截断，丢内容；如果只在一处剥但不一致（比如忘了给 history 剥），下一次 turn 的 prompt 里就会泄漏推理内容。s08 的契约很死：`sanitize` 只有 `BuildMessages` 一个调用点。

## Solution / 解决方案

新增 `context.go`：

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

// 内部：
func sanitize(text string) string
func sanitizeMessages(msgs []Message) []Message
func trimHistoryByTokens(msgs []Message, maxTokens int) []Message
func approximateTokens(m Message) int
```

`BuildSystemPrompt` 拼 4 段：

| 段 | 来源 | 标题 |
|---|---|---|
| 1 | `MemoryStore.ReadMemory` 遍历 `BootstrapFiles` | `## AGENTS.md` / `## SOUL.md` / ... |
| 2 | `MemoryStore.ReadMemory("MEMORY.md")` | `# Memory` |
| 3 | `SkillLoader.LoadForContext(SkillsRequested)` | `# Skills` |
| 4 | `RuntimeMeta` 渲染 | 围栏 `[Runtime Context — metadata only]` ... `[/Runtime Context]` |

空段（文件不存在 / skills 列表空）整段省略，不留 `---` 残骸。

`BuildMessages` 输出"裁剪过的历史 + 当前 user message"。**它不输出 system 消息**——system prompt 走 `RunSpec.System`，不是 `messages[0]`。这条决定看上去微妙，但意义大：Anthropic 协议把 system 当作顶层参数，不是 messages 数组里的一项；s04 早就分了这两条，s08 终于让 `RunSpec.System` 派上用场。

历史裁剪两步：

1. `Session.GetHistory(MaxHistoryMessages)` 取尾部 N 条；
2. `trimHistoryByTokens(msgs, MaxSystemTokens)` 用 `len(s)/4` 估算令牌，从最旧的开始丢直到符合预算。

`len(s)/4` 是占位符——s10 引入正经的 tiktoken 风格估算器。这个章节先把"两步裁剪"的形状钉死，s10 替换底层估算器时不用动 BuildSpec/ContextBuilder 的形状。

## How It Works / 工作原理

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
│        InitialMessages: msgs,        ← BuildMessages 出           │
│        System:          sysPrompt,   ← BuildSystemPrompt 出       │
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
│    parts.append(buildRuntimeContext(rt))   ← 总是有              │
│    return strings.Join(parts, "\n\n---\n\n")                     │
│                                                                  │
│  BuildMessages(spec):                                            │
│    history := spec.Session.GetHistory(spec.MaxHistoryMessages)   │
│    history = trimHistoryByTokens(history, spec.MaxSystemTokens)  │
│    history = sanitizeMessages(history)   ← 唯一一次              │
│    user := Message{Role: "user", Content: [{                     │
│        Type: "text", Text: sanitize(spec.UserText)               │
│    }]}                                                           │
│    return append(history, user)                                  │
│                                                                  │
│  ┌────────── Single sanitize boundary ──────────────────────┐    │
│  │                                                          │    │
│  │  Runner            : 不调 sanitize                       │    │
│  │  Hook (s09)        : 不调 sanitize                       │    │
│  │  MemoryStore       : 不调 sanitize（BuildMessages 已剥）  │    │
│  │  Session.Append    : 不调 sanitize                       │    │
│  │                                                          │    │
│  │  唯一调用点：context.go::BuildMessages                    │    │
│  └──────────────────────────────────────────────────────────┘    │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

核心约 50 行（节选自 [`agents/s08-context-builder/context.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s08-context-builder/context.go)）：

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

**5 个非显然之处**：

1. **`RuntimeMeta.Now` 是注入的，不是 `time.Now()` 直接读**。测试要确定性——每次 build 都读墙钟会让快照测试不可能。`main.go` 注入 `time.Now()`，测试注入 `fixedTime`，同一份代码两边都跑得通。
2. **system prompt 不进 `messages` 数组**。s04 把 `System` 字段独立出来，s08 终于让它有内容。Anthropic API 把 system 当顶层参数，OpenAI 也允许（虽然历史上塞 messages[0] 也行），上游 nanobot 因为兼容老 OpenAI shape 把它放 messages，但**learn 版本不必延续这个折中**——我们走更干净的路。
3. **`<think>` 剥掉只有一处**。`sanitize` 在 `context.go::BuildMessages` 调用，其他文件都不调。这条规则不是"建议"，是契约——anti-pattern #4 把它列为 production bug 来源。`sanitize` 本身是**幂等**的（连跑两次结果一样），第 7 个测试就盯着这条不变量。
4. **空段不留 `---` 残骸**。如果 `SOUL.md` 不存在，prompt 里就不要出现 `## SOUL.md` 标题或孤立的分隔符——LLM 看到一串空标题会奇怪。`parts` 是个 slice，每段的添加都先判 `!= ""`，最后 `strings.Join` 自然就紧凑了。
5. **两步历史裁剪，不能合并**。先按消息数（防"50 条短消息把上下文挤满"），再按令牌预算（防"3 条巨长消息超出窗口"）。两条限制语义不同，合成一个数字会丢信息。MaxHistoryMessages=0 时 `GetHistory` 退化成默认 120；MaxSystemTokens=0 时 `trimHistoryByTokens` 直接返回所有——两条独立的逃生口。

## What Changed / 与上一节的变化

```diff
  // s07/main.go：bus.Send 直接喂 raw history + userMsg
- bus.Send(InboundMessage{SessionKey, UserText: prompt, ID: "cli-1"})
- out := <-bus.Out()
- mem.AppendHistory(...)

  // s08/main.go：bus 不再用，main 直接驱动 ctxBuilder + Runner
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

新增类型（s08 的核心）：

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

`Runner` / `Provider` / `Tool` / `Registry` / `Bus` / `SessionManager` / `MemoryStore` / `SkillLoader` 全部一行没改。s08 是**纯组合**——把已有零件按新顺序拼起来。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s08-context-builder

# 跑一次 verbose，看到 system prompt 的真实大小
go run . -workspace /tmp/agent-demo -skills weather,echo -v "Tokyo weather"
# [s08] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s08] loaded 2 skill(s) ...
# [s08] skills requested for system prompt: [weather echo]
# [s08] system prompt: 612 chars; messages: 1
```

加一份 SOUL.md 让 system prompt 真正长出来：

```bash
cat > /tmp/agent-demo/SOUL.md <<'EOF'
You are a terse weather-savvy assistant. Always respond in 2 lines or less.
EOF

go run . -workspace /tmp/agent-demo -skills weather -v "Tokyo"
# [s08] bootstrap files present: 1/4 ...
# [s08] system prompt: 893 chars; messages: 1     ← 612 → 893
```

试 `<think>` 剥除（最直观的演示）：

```bash
go run . -workspace /tmp/agent-demo "<think>secret reasoning</think>visible question"
# 模型看到的 user 消息只有 "visible question"——<think> 块在 BuildMessages 里被剥掉了。
```

跑测试（race detector 必开）：

```bash
go test -count=1 -race -v ./...
```

期望 7 个测试全过：

- `BuildsSystemFromBootstrapFiles` — 写 SOUL.md / USER.md，断言 prompt 含 `## SOUL.md` / `## USER.md` 标题和正文，AGENTS.md / TOOLS.md 不在；
- `RuntimeContextBlock_Present` — 断言 `[Runtime Context — metadata only]` / `[/Runtime Context]` 围栏 + Channel/Sender/ChatID 都在；
- `HistoryTrimmed_RespectingMaxMessages` — 100 条历史 + MaxHistoryMessages=20 → 21 条（20 history + 1 current）；
- `Sanitize_StripsThinkTags` — 既测 UserText 也测 history 里的 assistant 文本；
- `NoSkillsConfigured_StillBuildsSystem` — 空 skill 列表也能产生 prompt；
- `HistoryTrimmed_RespectingTokenBudget` — `len(s)/4` 估算下，预算 25 token 大约保留 2-3 条 history；
- `Sanitize_DoubleStripIsIdempotent` — 跑两次 sanitize 结果一样（防御性测试，盯死单边界契约）。

## Upstream Source Reading / 上游源码阅读

s08 对应 `nanobot/agent/context.py`——整文件 ~210 行。我们把 `ContextBuilder.build_system_prompt`、`_build_runtime_context`、`_build_user_content`、`BOOTSTRAP_FILES` 翻进 Go，砍掉 `_get_identity`（它在上游里渲染 platform 信息 + workspace path 模板，我们 learn 版的 system prompt 可以更简洁）和 `_is_template_content`（避免把"用户没改过的 bundle 模板"也塞进 prompt 的优化路径，复杂度对教学不划算）。

完整注解版在 [`upstream-readings/s08-context-builder.py`](../../upstream-readings/s08-context-builder.py)。

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

**5 条对照阅读要点**：

- **上游 `BOOTSTRAP_FILES` 顺序与 learn 版一致**：AGENTS.md / SOUL.md / USER.md / TOOLS.md。每个文件的角色：AGENTS = 操作规程，SOUL = 人格，USER = 用户偏好，TOOLS = 自定义工具说明（如果有）。我们 Go 版把这个常量定义在 `memory.go`（s06），与 `MemoryStore.ReadMemory` 同源，s08 直接引用。
- **上游 `_RUNTIME_CONTEXT_TAG` 字符串里有连字符 `—`（U+2014）**——不是普通短横线 `-`。这是上游故意的：让 LLM 在训练数据里看到的"runtime context"标志固定，避免被语义上等价但字面不同的标签搅乱。我们 Go 版同样用 U+2014。
- **上游 `build_system_prompt` 里有 `_is_template_content` 检查**——如果 MEMORY.md 内容和 bundle 里附带的模板 byte-identical，就跳过它。这是为了避免新装的 agent 把"# Memory\n\n_The agent has no memory yet._"塞进每个 prompt。learn 版选择直接用 `strings.TrimSpace(body) == ""` 的更朴素判据——它够用，且不需要打包模板文件。
- **上游 `build_messages` 把 system prompt 塞进 `messages[0]`**——这是 OpenAI API 早期支持的 shape，Anthropic 后来要求把 system 提到顶层参数。learn 版从 s04 起就走 Anthropic shape（`RunSpec.System` 独立字段），所以 `BuildMessages` 不返回 system 消息。
- **上游 `_build_user_content` 处理 base64 图片**——`media: list[str]` 是图片路径列表，`detect_image_mime` 嗅探 magic bytes 拼成 data URL。learn 版 s08 没做多模态，只接 `UserText string`；图片是 Phase G 的内容。

**想读更多**：

1. `nanobot/agent/context.py:_get_identity` —— 上游 system prompt 还有一段"identity"模板，渲染 workspace 路径 + 操作系统 + Python 版本。看这个能体会到 nanobot 把 *运行环境* 也当 prompt 的一部分。
2. `nanobot/agent/context.py:_merge_message_content` —— 上游会合并连续同 role 的消息（避免某些 provider 拒绝），这是 ChatReq 兼容性的细节。learn 版当前不需要，但 s11 接上 Hook 之后可能要。
3. `nanobot/utils/prompt_templates.py` —— 上游用 Jinja2 风格的 template 引擎渲染 `agent/identity.md` / `agent/skills_section.md`。learn 版选择把模板文本直接写在 Go 里（更显式），但你看完上游会理解为什么 production 用模板。

---

**下一节预告**：s09 给 `Runner` 加 `Hook` 接口——5 个生命周期点（BeforeIter / OnStream / BeforeExecuteTools / AfterIter / FinalizeContent），加 `CompositeHook`（fan-out + 错误隔离）和 `SDKCaptureHook`（累积 tools_used）。Runner 在每个 phase 调对应的 hook，让外部代码能观察、改写、甚至中断 turn。

Next: s09 makes the Runner observable via Hook callbacks.
