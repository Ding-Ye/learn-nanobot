---
title: "s04 · Agent runner（内部工具循环）"
chapter: 4
slug: s04-agent-runner
est_read_min: 22
---

# s04 · Agent runner（内部工具循环）

> 教什么：把 s03 的 `Loop` 内核切出来变成 `Runner`——只关心"调 LLM ↔ 跑 tools ↔ 重复"，不再负责 prompt 拼装。这是为 s11 的外层状态机让路。

---

## Problem / 问题

s03 的 `Loop` 把两件不同的事捏在一起：

1. **驱动 LLM**——provider.Chat → 拿 LLMResponse → 跑 tools → 再 Chat → 直到 stop
2. **构造初始消息**——`messages := [{role: user, content: prompt}]` 这一行是 Loop 自己写死的

这俩在小项目里没冲突，但上游 nanobot 的架构里它们是两层：内层叫 `AgentRunner`（runner.py），外层叫 `AgentLoop`（loop.py）。`AgentLoop` 处理 RESTORE / COMPACT / BUILD / SAVE / RESPOND 等"一次会话生命周期"的状态机，`AgentRunner` 只负责其中 RUN 这一格的"LLM ↔ tools 循环"。

我们 s11 会把那个外层状态机重新引入并叫它 `Loop`。所以 s04 必须先**把这个名字让出来**——把 s03 的 `Loop` 改名 `Runner`，把"构造初始消息"的责任剥离出去（让 caller / s08 的 ContextBuilder 去做），并把返回值从一个 `string` 升级成结构化的 `AgentRunResult`。

顺便引入"length 恢复"——上游 `_handle_length_recovery` 的最小教学版。

## Solution / 解决方案

新接口：

```go
type AgentRunSpec struct {
    InitialMessages []Message    // caller 已经构造好的消息列表
    Tools           *Registry
    Model           string
    MaxIterations   int
    System          string
}

type AgentRunResult struct {
    FinalContent string         // 最后一条 assistant 文本
    Messages     []Message      // 完整消息日志（供 session 持久化）
    ToolsUsed    []string       // 去重后的 tool 名字列表（telemetry）
    Usage        Usage           // 累计 token 用量
    FinishReason string         // "stop" / "length" / "max_iterations" / "error"
}

type Runner struct { Provider Provider }
func NewRunner(p Provider) *Runner
func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error)
```

三个关键决策：

1. **Caller 构造 InitialMessages**。`Runner.Run` 不再写 `[{user, prompt}]`。这是 s08 ContextBuilder 接入的关键 seam——它会拼好 `[system, memory, skills, history, user]` 之后整个传进来。
2. **结构化 AgentRunResult**。s05 的 SessionManager 要持久化整个 message log；s09 的 hooks 要看到 token 累计；s11 的 SAVE 状态要拿到 `tools_used`。返回 `string` 是不够的。
3. **Length 恢复，但只一次**。上游 `_MAX_LENGTH_RECOVERIES = 3`；我们做 1 次。当 `FinishReason == "length"` 时，append 一条"continuation hint" user message，让下一轮重新提示。两次 `length` 还没好就 bail。

## How It Works / 工作原理

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────────┐
│                                                                │
│  caller (main / Bus / Loop) builds AgentRunSpec               │
│  ─► spec.InitialMessages = [{user: prompt}]   ← 不再 Runner 做 │
│                                                                │
│  Runner.Run(ctx, spec):                                       │
│    for iter := 0; iter < spec.MaxIterations; iter++ {         │
│      resp, _ := provider.Chat(ChatReq{messages, tools, model})│
│      messages = append(messages, assistant: resp.Blocks)      │
│                                                                │
│      switch resp.FinishReason {                               │
│        case "stop":                                            │
│          return AgentRunResult{                               │
│             FinalContent, Messages, ToolsUsed, Usage, "stop"} │
│        case "tool_calls":                                      │
│          results := registry.Execute each ToolCall            │
│          messages = append(user: tool_results)                │
│          ToolsUsed += new tool names                          │
│        case "length":                                          │
│          if no recoveries left: bail with FinishReason="length"│
│          messages = append(user: "your reply was truncated…")  │
│          // next iter re-prompts                               │
│        case "error":                                           │
│          return wrapped error (retry hint preserved)          │
│      }                                                         │
│    }                                                           │
│    return error "max iterations exceeded" (FinishReason       │
│           = "max_iterations")                                  │
│                                                                │
└────────────────────────────────────────────────────────────────┘
```

核心 ~30 行（节选自 [`agents/s04-agent-runner/runner.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s04-agent-runner/runner.go)）：

```go
func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
    messages := append([]Message(nil), spec.InitialMessages...)
    usage, toolsUsed, seen := Usage{}, []string{}, map[string]bool{}
    lengthRecoveriesLeft := 1

    for iter := 0; iter < spec.MaxIterations; iter++ {
        resp, err := r.Provider.Chat(ctx, ChatReq{
            Messages: messages, Tools: spec.Tools.Definitions(),
            Model: spec.Model, System: spec.System,
        })
        if err != nil { return AgentRunResult{Messages: messages, FinishReason: "error"}, err }
        if resp.FinishReason == "error" { /* bail with retry hint */ }

        usage.InputTokens += resp.Usage.InputTokens
        usage.OutputTokens += resp.Usage.OutputTokens
        messages = append(messages, Message{Role: "assistant", Content: resp.AssistantBlocks})

        switch resp.FinishReason {
        case "stop":
            return AgentRunResult{FinalContent: resp.Content, Messages: messages,
                ToolsUsed: toolsUsed, Usage: usage, FinishReason: "stop"}, nil
        case "tool_calls":
            results := r.runToolCalls(ctx, spec.Tools, resp.ToolCalls, iter, spec.Verbose)
            messages = append(messages, Message{Role: "user", Content: results})
            for _, c := range resp.ToolCalls {
                if !seen[c.Name] { seen[c.Name] = true; toolsUsed = append(toolsUsed, c.Name) }
            }
        case "length":
            if lengthRecoveriesLeft <= 0 { /* bail */ }
            lengthRecoveriesLeft--
            messages = append(messages, Message{Role: "user", Content: []ContentBlock{{
                Type: "text",
                Text: "Your previous response was truncated. Please continue and complete it.",
            }}})
        }
    }
    return AgentRunResult{Messages: messages, ToolsUsed: toolsUsed, Usage: usage,
        FinishReason: "max_iterations"}, fmt.Errorf("runner: max iterations (%d) exceeded", spec.MaxIterations)
}
```

**4 个非显然之处**：

1. **`InitialMessages` 是 slice copy 不是引用**。我们 `append([]Message(nil), spec.InitialMessages...)` 复制一份再用。如果直接持有 caller 的 slice，多次 append 可能踩到 caller 的底层数组——s05 的 Bus 有多个 goroutine，每个 goroutine 调 Run，必须各自隔离。
2. **`ToolsUsed` 是去重后的有序列表**。多次调用同一个 tool（典型场景：`bash` 调 5 次），上游的 `tools_used` 也是 dedup 的；我们用 `seenTool` map 实现。
3. **Length 恢复用 user message 而不是 system**。Anthropic 协议要求 system 只能在 ChatReq 顶层；中间塞 user message "请继续" 是合法的，且 LLM 会把它当成对话延续。上游 `build_length_recovery_message()` 也是这个形态。
4. **`tool_result` 的 `ToolUseID` 必须匹配 `tool_use.ID`**。Provider 已经把 ID 透传到 `ToolCall.ID`；Runner 把它写回 `ContentBlock.ToolUseID`。这条对应关系一旦断了，Anthropic 会 422 拒绝。

## What Changed / 与上一节的变化

```diff
  // s03/loop.go
- type Loop struct {
-     Provider  Provider
-     Tools     *Registry
-     MaxTurns  int
-     Model     string
-     System    string
-     Verbose   bool
- }
- func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
-     messages := []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: userPrompt}}}}
-     for turn := 0; turn < l.MaxTurns; turn++ { ... }
-     return resp.Content, nil   // 只返回字符串
- }

  // s04/runner.go
+ type AgentRunSpec struct {
+     InitialMessages []Message
+     Tools           *Registry
+     Model           string
+     MaxIterations   int
+     System          string
+ }
+ type AgentRunResult struct {
+     FinalContent string
+     Messages     []Message     // 整个消息日志，给 session 持久化
+     ToolsUsed    []string      // 给 telemetry / hooks
+     Usage        Usage         // 累计 token
+     FinishReason string        // stop / length / max_iterations / error
+ }
+ type Runner struct { Provider Provider }
+ func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
+     messages := append([]Message(nil), spec.InitialMessages...)  // caller 已构造
+     for iter := 0; iter < spec.MaxIterations; iter++ {
+         // 同样的 switch 三分支 (+ "length" 多了 recovery 分支)
+     }
+ }
```

`main.go` 也变了——caller 现在自己写第一条 user message：

```diff
- final, err := loop.Run(ctx, prompt)
+ result, err := runner.Run(ctx, AgentRunSpec{
+     InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: prompt}}}},
+     Tools: registry, Model: model, MaxIterations: maxTurns, Verbose: verbose,
+ })
+ fmt.Println(result.FinalContent)
```

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s04-agent-runner

go run . -v "list .go files"

# 测试覆盖了 stop / 两 tool_calls / max iterations / 部分失败 / length 恢复 / provider 错误
go test -v ./...

# 故意触发一次 quota 错误（401 because key wrong），看 Runner 报错形态
ANTHROPIC_API_KEY=invalid go run . "test" 2>&1 | head -5
```

期望输出形态：

```
$ go run . -v "list .go files"
[s04] model=claude-sonnet-4-6 tools=1 max_iterations=20
[iter 0] assistant: I'll list the .go files.
[iter 0] -> bash map[command:ls *.go]
[iter 0] <- bash_tool.go main.go provider.go provider_anthropic.go provider_fake.go registry.go runner.go tools.go
[iter 1] assistant: Eight .go files: ...
Eight .go files: ...
[s04] tools_used=[bash] input_tokens=1523 output_tokens=84 finish=stop
```

最后一行是 s04 新增的——verbose 模式打印 `tools_used` + token 累计。

## Upstream Source Reading / 上游源码阅读

上游 `nanobot/agent/runner.py` 才是这一节的本体。完整注解版在 [`upstream-readings/s04-agent-runner.py`](../../upstream-readings/s04-agent-runner.py)。

```upstream:nanobot/agent/runner.py#L55-L99
@dataclass(slots=True)
class AgentRunSpec:
    """Configuration for a single agent execution."""
    initial_messages: list[dict[str, Any]]
    tools: ToolRegistry
    model: str
    max_iterations: int
    max_tool_result_chars: int
    temperature: float | None = None
    max_tokens: int | None = None
    reasoning_effort: str | None = None
    hook: AgentHook | None = None
    error_message: str | None = _DEFAULT_ERROR_MESSAGE
    max_iterations_message: str | None = None
    concurrent_tools: bool = False
    fail_on_tool_error: bool = False
    workspace: Path | None = None
    session_key: str | None = None
    context_window_tokens: int | None = None
    context_block_limit: int | None = None
    provider_retry_mode: str = "standard"
    progress_callback: Any | None = None
    stream_progress_deltas: bool = True
    retry_wait_callback: Any | None = None
    checkpoint_callback: Any | None = None
    injection_callback: Any | None = None
    llm_timeout_s: float | None = None


@dataclass(slots=True)
class AgentRunResult:
    """Outcome of a shared agent execution."""
    final_content: str | None
    messages: list[dict[str, Any]]
    tools_used: list[str] = field(default_factory=list)
    usage: dict[str, int] = field(default_factory=dict)
    stop_reason: str = "completed"
    error: str | None = None
    tool_events: list[dict[str, str]] = field(default_factory=list)
    had_injections: bool = False
```

**5 条对照阅读要点**：

- **上游 `AgentRunSpec` 有 25 个字段，我们 5 个**。剪掉的字段几乎全都是后续 chapter 的责任：`hook`（s09）、`progress_callback`（s09 的流式）、`workspace` / `session_key`（s05/s06）、`max_tool_result_chars`（s10 的截断）、`injection_callback`（高级 sideband 注入）、`provider_retry_mode`（不在我们教学范围）。教学时一次只引一两个新概念。
- **`fail_on_tool_error: bool` 我们没做**。我们的 `Registry.Execute` 永远把工具异常转成 `"tool error: ..."` 字符串塞回 tool_result——LLM 自己决定怎么应对。production 的 nanobot 允许把 `fail_on_tool_error=True` 让 Runner 直接 abort（适合 batch 模式）。延伸练习。
- **`concurrent_tools: bool` 我们没做**。LLM 一次返回多个 tool_use blocks 时上游可以并发执行（asyncio.gather）；我们顺序执行。Go 的并发更便宜，但教学版顺序执行更易读。
- **`_handle_length_recovery` 上游做 3 次**（`_MAX_LENGTH_RECOVERIES = 3`），且会用 `build_length_recovery_message()` 模板生成更精细的提示。我们 1 次 + 写死字符串。production 想要更强 recovery 时把 `lengthRecoveriesLeft = 1` 调成 3 + 把 hint 字符串换成模板就行。
- **`AgentHookContext`（s09）会在 `before_iteration` / `on_stream` / `before_execute_tools` / `after_iteration` 触发**——上游的 Runner 在循环各点都 await hook 回调。我们 s04 完全没 hook；s09 会回头给 Runner 加上对应的钩子函数。

**想读更多**：

1. `nanobot/agent/runner.py:200-400` 看完整的 `AgentRunner.run()`——尤其 `_handle_length_recovery`、`_drain_injections`、`_emit_checkpoint` 这三个 helper。
2. `nanobot/utils/runtime.py::build_length_recovery_message` 看上游的 hint 模板；它会把上一段不完整的回复自身也抄回去当 context。
3. `nanobot/agent/loop.py::_state_run` 看外层状态机怎么调 Runner——这是 s11 的核心阅读对象。

---

**下一节预告**：s05 把 Runner 包到一个 per-session goroutine + MessageBus 里，这样多个 session 可以并行跑而同一 session 内的 turn 是串行的。Runner 形状不变——Bus 只是从每个 session goroutine 里调 `runner.Run(ctx, spec)`。
