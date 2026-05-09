---
title: "s09 · Hook 系统"
chapter: 9
slug: s09-hooks
est_read_min: 22
---

# s09 · Hook 系统

> 教什么：真实 agent 需要生命周期可观测性。Runner 在 s08 之前是个"黑盒"——调它，拿结果，祈祷别炸。s09 把它撬开，加 5 个挂载点：`BeforeIter`、`OnStream`、`BeforeExecuteTools`、`AfterIter`、`FinalizeContent`。`CompositeHook` 把多个 hook 扇出（fan-out），并且做**错误隔离**（一个 hook panic 不能干掉别的）。`SDKCaptureHook` 是上游提供的"默认观测 hook"，让 SDK 调用方不用自己写 hook 就能拿到 `tools_used` 和 final messages。Hook 是流式输出、telemetry、中途取消、sideband 注入的共同接缝（这四个都是延伸练习）。

---

## Problem / 问题

到 s08 为止，Runner 是个封闭循环。你给它 `RunSpec.InitialMessages + RunSpec.System + RunSpec.Tools`，它给你 `AgentRunResult.FinalContent + Messages + ToolsUsed + Usage`。如果你想：

- 把每一轮的 token 用量打到 metrics endpoint；
- 把 assistant 的部分 token 流式推到 websocket，让 UI 显示进度；
- 用户点了"停止"按钮，turn 要中途取消；
- 在轮次之间塞一条 sideband 指令（"用户在你思考的时候又发了一条——请并入"）；
- 在最终响应回去之前先脱敏 PII；

——目前都做不到。Runner 没暴露任何 hook，没有任何回调。唯一信号就是返回值。

上游的答案是 `nanobot/agent/hook.py`：定义 `AgentHook` 接口（5 个生命周期方法，加一个 `on_stream_end` 我们 s09 折叠进 `OnStream`），加 `AgentHookContext` 这个共享给 hook 实现的可变 bag。`CompositeHook` 让你把多个 hook 叠在一个 Runner 上。`SDKCaptureHook` 是默认观测 hook，SDK 调用方不用自己写。

有意思的不是接口，而是**错误隔离**策略。如果某个自定义 hook 在 `before_iteration` 里 panic 了，run 不应该死。其它注册的 hook 还应该收到回调。组合层捕异常、按 hook 按方法逐个隔离、log 出来。这条规则有一个例外——`finalize_content` 是个**流水线**（每个 hook 的输出是下一个的输入），那里 panic 应该响亮地失败，因为 finalize bug 能以"很要命"的方式破坏响应。

s09 把这条策略一字不差地搬过来，外加一个 Go 风格细节：panic 取代 try/except。组合层用 `recover()` 按 hook 按方法逐个隔离、把恢复的值 log 出来、并把聚合的 error 抛回 Runner，让调用方能听见。

最微妙的测试设计选择是：recording hook 必须看到精确正确的 call 顺序。乱序会在 production 静默地破坏观测 hook。所以第一个测试钉死 2-turn 循环的顺序：`[BeforeIter(0), BeforeExecuteTools(0), AfterIter(0), BeforeIter(1), AfterIter(1), FinalizeContent]`。以后哪天你重构 Runner，不小心把 `AfterIter` 提到 tool 执行之前，这个测试就会立刻喊出来。

## Solution / 解决方案

`hook.go` 里 3 个新类型：

```go
type Hook interface {
    BeforeIter(ctx context.Context, hc *HookCtx) error
    OnStream(ctx context.Context, hc *HookCtx, delta string) error
    BeforeExecuteTools(ctx context.Context, hc *HookCtx) error
    AfterIter(ctx context.Context, hc *HookCtx) error
    FinalizeContent(ctx context.Context, hc *HookCtx, content string) string
}

type HookCtx struct {
    Iter      int
    Messages  []Message
    Resp      *LLMResponse
    ToolCalls []ToolCall
    Usage     Usage
    Err       error
}

type CompositeHook struct {
    hooks    []Hook
    LogPanic func(method string, h Hook, recovered any)
}
```

外加 3 个小帮手：

```go
type NoopHook struct{}            // Runner 没设 Hook 时的默认值
type SDKCaptureHook struct {      // 上游的默认观测 hook
    ToolsUsed     []string
    FinalMessages []Message
}
func NewCompositeHook(hs ...Hook) *CompositeHook
func (c *CompositeHook) Add(h Hook)
```

`Runner` 加 `Hook` 字段。`Runner.Run` 在每轮的 5 个明确时刻调 hook：

1. `BeforeIter` —— 每轮开头，LLM 调用之前。返回非 nil error 短路 run。
2. （LLM 调用）
3. `BeforeExecuteTools` —— 只在 LLM 要工具时触发。返回非 nil error 干净地短路这一轮（工具不跑）。
4. （工具执行）
5. `AfterIter` —— 每轮结尾，无论结尾是 stop / tool_calls / length / error。

循环结束后，一次：

6. `FinalizeContent` —— hook 的返回值替换 Runner 给调用方的内容。

`OnStream` 在接口上但 Runner 暂时不调——流式是延伸练习。把方法留在接口上，是为了让以后写流式的人不用改 `Hook`。

3 个测试 fixture 覆盖完整面：

- `recordingHook` 把每个方法记成 `Method(iter)` 用于断言顺序；
- `panicHook` 在 `BeforeIter` 里 panic，测错误隔离；
- `rewriteHook`（嵌入 `NoopHook`）在 `FinalizeContent` 里加 "rewritten:" 前缀；
- `shortcircuitHook`（嵌入 `NoopHook`）在 `BeforeExecuteTools` 里返回 sentinel error。

## How It Works / 工作原理

```ascii-anim frames=4
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  Runner.Run                                                      │
│    hook := r.Hook                                                │
│    if hook == nil { hook = NoopHook{} }                          │
│    hc := &HookCtx{}                                              │
│                                                                  │
│    for iter := 0; iter < MaxIterations; iter++ {                 │
│        hc.Iter = iter                                            │
│        hc.Messages = messages                                    │
│        hc.Resp = nil; hc.ToolCalls = nil                         │
│                                                                  │
│        ╔══ BeforeIter ══╗  ← 挂载点 #1                            │
│        if err := hook.BeforeIter(ctx, hc); err != nil { return } │
│                                                                  │
│        resp, _ := provider.Chat(ctx, ...)                        │
│        hc.Resp = &resp; hc.ToolCalls = resp.ToolCalls            │
│        hc.Messages = messages // 刷新                             │
│                                                                  │
│        switch resp.FinishReason {                                │
│        case "stop":                                              │
│            ╔══ AfterIter ══╗  ← 挂载点 #5                         │
│            hook.AfterIter(ctx, hc)                               │
│            ╔══ FinalizeContent ══╗  ← 挂载点 #6                   │
│            return hook.FinalizeContent(ctx, hc, resp.Content)    │
│                                                                  │
│        case "tool_calls":                                        │
│            ╔══ BeforeExecuteTools ══╗  ← 挂载点 #3                │
│            if err := hook.BeforeExecuteTools(...); err != nil {  │
│                hook.AfterIter(...)  // 仍触发，给观测 hook 看      │
│                return                                            │
│            }                                                     │
│            // 跑工具，结果追加到 messages                          │
│        }                                                         │
│        ╔══ AfterIter ══╗  ← 挂载点 #5（每轮）                     │
│        hook.AfterIter(ctx, hc)                                   │
│    }                                                             │
│                                                                  │
│  CompositeHook fan-out（除 finalize 外的所有方法）:                │
│    for h := range c.hooks {                                      │
│        defer func() {                                            │
│            if r := recover(); r != nil {                         │
│                c.LogPanic(method, h, r)                          │
│                err = fmt.Errorf("hook %s panic in %T: %v", ...)  │
│            }                                                     │
│        }()                                                       │
│        err := h.method(ctx, hc, ...)                             │
│        errs = append(errs, err)                                  │
│    }                                                             │
│    return joinErrs(errs)                                         │
│                                                                  │
│  CompositeHook.FinalizeContent（流水线，无 recover）:              │
│    for h := range c.hooks {                                      │
│        content = h.FinalizeContent(ctx, hc, content)             │
│    }                                                             │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

核心约 40 行（节选自 [`agents/s09-hooks/hook.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s09-hooks/hook.go)）：

```go
func (c *CompositeHook) safeCall(method string, h Hook, fn func() error) (err error) {
    defer func() {
        if r := recover(); r != nil {
            if c.LogPanic != nil {
                c.LogPanic(method, h, r)
            } else {
                fmt.Printf("[hook %s panic in %T]: %v\n", method, h, r)
            }
            err = fmt.Errorf("hook %s panic in %T: %v", method, h, r)
        }
    }()
    return fn()
}

func (c *CompositeHook) BeforeIter(ctx context.Context, hc *HookCtx) error {
    errs := make([]error, 0, len(c.hooks))
    for _, h := range c.hooks {
        err := c.safeCall("BeforeIter", h, func() error { return h.BeforeIter(ctx, hc) })
        errs = append(errs, err)
    }
    return joinErrs(errs)
}

func (c *CompositeHook) FinalizeContent(ctx context.Context, hc *HookCtx, content string) string {
    for _, h := range c.hooks {
        content = h.FinalizeContent(ctx, hc, content)
    }
    return content
}
```

**5 个非显然之处**：

1. **`HookCtx` 是可变 bag，不是不可变参数**。对应上游的 `AgentHookContext` dataclass。Runner 在一次 Run 里用同一个 `*HookCtx` 反复调 hook。这样字段增长（s10 加 usage drift；s11 加 turn state）时方法签名不变，hook 各取所需。

2. **错误隔离按方法走，不一刀切**。fan-out 方法 recover panic；`FinalizeContent` 不 recover。为什么不对称？因为 finalize 是流水线——每个 hook 的输出喂给下一个。中间某个 hook panic 后被静默丢掉，下游 hook 看到的是损坏的输入。Finalize bug 应该响亮地挂掉。其它方法都是观察者，隔离它们既安全又必要。

3. **`AfterIter` 永远会触发，即使出错**。最朴素的实现会在 `BeforeExecuteTools` 返 error 时跳过 `AfterIter`。我们故意让它仍触发，让观测 hook 看到这次失败。`_ = hook.AfterIter(...)` 中的 `_` 表示"不在乎它的 error 返回值；这一轮已经因为别的原因挂了"。

4. **`OnStream` 在接口上但还没人调**。把它留在接口上意味着以后某个 session 接上流式时，不需要破坏所有现有 hook 实现。如果不留，后来加，每个 `Hook` 实现都得改。一个未用方法的代价是每个 hook 多一个 no-op 行；接口变更的代价是下游代码全坏。

5. **Runner 用单个 `Hook`，不用 `[]Hook`**。组合通过 `CompositeHook`，不通过 Runner。这让 Runner 简单（每个时刻一次调用，不是循环），让组合在调用点显式。要 telemetry + capture？`runner.SetHook(NewCompositeHook(telemetry, capture))`。只要 capture？`runner.SetHook(capture)`。复合是"一次多 hook"的标准模式。

## What Changed / 与上一节的变化

```diff
  // s08/runner.go：Runner 只有 Provider；Run 不调 hook
- type Runner struct { Provider Provider }
- func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
-     for iter := 0; iter < spec.MaxIterations; iter++ {
-         resp, err := r.Provider.Chat(ctx, ChatReq{...})
-         // 跑工具
-     }
- }

  // s09/runner.go：Runner 多了 Hook；Run 在 5 个点调 hook
+ type Runner struct {
+     Provider Provider
+     Hook     Hook
+ }
+ func (r *Runner) SetHook(h Hook) { r.Hook = h }
+ func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
+     hook := r.Hook
+     if hook == nil { hook = NoopHook{} }
+     hc := &HookCtx{}
+     for iter := 0; iter < spec.MaxIterations; iter++ {
+         hc.Iter = iter
+         if err := hook.BeforeIter(ctx, hc); err != nil { ... }
+         resp, _ := r.Provider.Chat(...)
+         hc.Resp = &resp; hc.ToolCalls = resp.ToolCalls
+         if resp.FinishReason == "tool_calls" {
+             if err := hook.BeforeExecuteTools(ctx, hc); err != nil { ... }
+             // 跑工具
+         }
+         hook.AfterIter(ctx, hc)
+     }
+     finalContent = hook.FinalizeContent(ctx, hc, finalContent)
+ }
```

新增类型（s09 的核心）：

```go
+ type Hook interface { ... 5 个方法 ... }
+ type HookCtx struct { Iter int; Messages []Message; Resp *LLMResponse; ... }
+ type NoopHook struct{}
+ type CompositeHook struct { hooks []Hook; LogPanic func(...) }
+ type SDKCaptureHook struct { ToolsUsed []string; FinalMessages []Message; mu sync.Mutex }
```

`Provider` / `Tool` / `Registry` / `Bus` / `SessionManager` / `MemoryStore` / `SkillLoader` / `ContextBuilder` 全部一行没改。s09 的接线只动了 `Runner.Hook` 字段加 5 个挂载点的插入。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s09-hooks

# verbose 跑，盯新的 [s09] hook recorded 行
go run . -workspace /tmp/agent-demo -skills weather -v "Tokyo weather"
# [s09] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s09] loaded 2 skill(s) ...
# [s09] system prompt: 612 chars; messages: 1
# [iter 0] -> bash {command:curl -s wttr.in/Tokyo?format=3}
# [iter 0] <- Tokyo: clear, 18°C
# [iter 0] assistant: ...
# [s09] hook recorded 1 tool invocation(s): [bash]
# Tokyo: clear, 18°C.
```

`[s09] hook recorded ...` 这一行是 SDKCaptureHook 工作过的外部信号。内部上，同一个 hook 也把 `FinalMessages` 抓下来给 SDK 调用方用。

跑测试（race detector 必开）：

```bash
go test -count=1 -race -v ./...
```

期望 6 个测试全过：

- `TestHook_BeforeIter_AfterIter_CalledInOrder` —— 钉死调用顺序；
- `TestCompositeHook_OneFails_OthersStillRun` —— 验证错误隔离；
- `TestSDKCaptureHook_AccumulatesToolsUsed` —— 验证默认观测 hook；
- `TestHook_FinalizeContent_CanRewrite` —— 验证 FinalizeContent 能改写；
- `TestHook_BeforeExecuteTools_CanShortcircuit` —— 验证短路语义；
- `TestRunner_NoHook_DefaultsToNoop` —— 不设 hook 时 Runner 仍能跑的回归测试。

## Upstream Source Reading / 上游源码阅读

s09 对应 `nanobot/agent/hook.py`——整文件 ~123 行。我们把 `AgentHookContext`、`AgentHook`、`CompositeHook`、`SDKCaptureHook` 翻成 Go。把 `on_stream_end` 折叠进 `OnStream`（Runner 暂时不驱动流式），把 `wants_streaming()` 砍掉（这是 Python 的 ergonomic——hook 说"给我开 OnStream"；Go 这边以后接流式时用 nil 检查就够了）。

完整注解版在 [`upstream-readings/s09-hooks.py`](../../upstream-readings/s09-hooks.py)。

```upstream:nanobot/agent/hook.py#L13-L27
@dataclass(slots=True)
class AgentHookContext:
    """Mutable per-iteration state exposed to runner hooks."""

    iteration: int
    messages: list[dict[str, Any]]
    response: LLMResponse | None = None
    usage: dict[str, int] = field(default_factory=dict)
    tool_calls: list[ToolCallRequest] = field(default_factory=list)
    tool_results: list[Any] = field(default_factory=list)
    tool_events: list[dict[str, str]] = field(default_factory=list)
    streamed_content: bool = False
    final_content: str | None = None
    stop_reason: str | None = None
    error: str | None = None
```

```upstream:nanobot/agent/hook.py#L75-L85
async def _for_each_hook_safe(self, method_name: str, *args: Any, **kwargs: Any) -> None:
    for h in self._hooks:
        if getattr(h, "_reraise", False):
            await getattr(h, method_name)(*args, **kwargs)
            continue

        try:
            await getattr(h, method_name)(*args, **kwargs)
        except Exception:
            logger.exception("AgentHook.{} error in {}", method_name, type(h).__name__)
```

**5 条对照阅读要点**：

- **上游 `AgentHookContext` 字段比 learn 的 `HookCtx` 多**——`tool_results`、`tool_events`、`streamed_content`、`stop_reason`、`final_content`。我们只挑 Runner 真正用到的子集。`tool_results` 已经隐含在 `Messages` 里（runner 把它们作为 user-role 的 tool_result 块追加）；`tool_events` 是给流式 MCP 工具准备的、我们没建；`streamed_content` 是个流式标志、目前不需要。如果以后 session 需要、加一个方法就能补齐。

- **上游 hook 是 `async`**；Go 这边是同步的。Go 的调用约定是"hook 阻塞 Runner 直到返回"。hook 要做 I/O 就自己 spawn goroutine。这是 Python 的 `async def` 翻 Go 的一般做法——同步默认更好教。

- **上游每个 hook 支持 `_reraise` 标志**；learn 不支持。在自定义 hook 上设 `_reraise=True` 让 `CompositeHook` 跳过 try/except、把异常抛出去。对希望 error 让 run 失败的测试有用。我们不带这个标志，因为想要 reraise 语义的测试可以直接注册 hook（不通过组合层）。

- **上游 `SDKCaptureHook` 只实现了 `after_iteration`**，不是完整接口。其它方法从 `AgentHook.__init__` 继承 no-op 默认值。Go 的接口没有继承，所以要么在 `SDKCaptureHook` 上显式实现每个方法（s09 这么做的），要么嵌入 `NoopHook` 拿默认值。两种都行，我们选显式因为 code review 时更可搜——`grep SDKCaptureHook` 能看到它实现的每个方法。

- **上游的 `finalize_content` 是 sync 的**，是 `AgentHook` 上唯一的 sync 方法。原因是 finalize 在所有 I/O 完成之后做，手里已经有最终字符串了，没什么可 await 的。Go 版自然继承了同步默认契约，免费。

**想读更多**：

1. `nanobot/agent/runner.py:_run_iteration` —— 上游 Runner 的 hook 集成。同样的 5 个挂载点，但周围控制流还要负责流式（`_run_streamed_iteration` vs `_run_blocking_iteration`）。读这个能体会到为什么上游接口里有 `wants_streaming()`。

2. `nanobot/agent/runner.py:_handle_streamed_iteration` —— 流式路径。Hook 的 `on_stream(delta)` 每个 delta token 调一次；`on_stream_end(resuming=True)` 在流式 iteration 结束但循环要继续时（LLM 要工具）触发。

3. `nanobot/utils/audit.py` —— production 的 audit hook。一个真实世界的 hook 实现，把每轮的 prompt + response log 到磁盘。读这个能体会 production 自定义 hook 长什么样：持有打开的 file handle、批量写、磁盘满时 fail-safe。

---

**下一节预告**：s10 —— Consolidator 与 AutoCompact，做记忆压缩。

Next: s10 — Consolidator + AutoCompact for memory compression.
