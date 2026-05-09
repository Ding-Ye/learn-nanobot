---
title: "s01 · 最小 agent loop"
chapter: 1
slug: s01-minimum-loop
est_read_min: 25
---

# s01 · 最小 agent loop

> 教什么：一个 LLM 你反复调用就是 agent。这个想法用 Go 写出来正好三个文件、~320 行。

---

## Problem / 问题

nanobot 上游是 ~5k 行 Python 写成的——10+ 个 LLM provider、Telegram/Slack/Discord channel、文件系统记忆、Dream consolidation、MCP 集成、技能系统、TurnState 状态机……刚开始读会被淹没：哪一段是"agent 的核心"？哪些是周边？

答案是：**真正的 agent 核心不到 50 行**。其它一切都是这 50 行的运行时支撑。这一节就是那 50 行——provider + 1 个工具 + 一个有 turn cap 的 for 循环。看完这一节你会有一个心智锚点：之后每加一节，都是给这 50 行外面加一层壳。

## Solution / 解决方案

agent loop 的心智模型只有四步：

1. 把用户消息塞进 `messages`
2. 调一次 LLM
3. 如果它请求工具调用，执行工具，把结果作为 *user* 消息塞回 `messages`，回到 step 2
4. 如果它说 "end_turn"，返回它最后的文字

加一个最大 turn 数防止无限循环，就是全部。

三个关键决策点：

1. **`Provider` 是接口不是实体。** 我们写的是 `interface { CreateMessage(...) }`。后续 s03 把这里换成可正常化的 `LLMResponse`，phase G 加 OpenAI-compat 实现，**`loop.go` 一行不用改**。
2. **`Tool` 也是接口。** s02 把工具走查从 `Loop` 里搬出来，s07 让 Markdown SKILL.md 也实现这个接口——这一切都因为 s01 这一刻把 `Tool` 写成了 interface 而不是 struct。
3. **工具的"运行错误"用字符串返回，不是 Go error。** 这是 nanobot 上游也这样做的设计——LLM 看见 stderr 后还能 reason；如果我们把错误抛回去，loop 就直接 abort 了，模型没机会"重试别的命令"。

## How It Works / 工作原理

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────┐
│                                                            │
│   Loop.Run(prompt) ──────────────────────► messages = [    │
│                                              {user, prompt}│
│                                            ]               │
│                                              │             │
│       ┌──────────────────────────────────────┘             │
│       │                                                    │
│       ▼   ┌─ provider.CreateMessage ─► resp                │
│   ╔══════╗                              │                  │
│   ║ for  ║◄────────  no tool_use ───────┘ → return text    │
│   ║ turn ║                                                 │
│   ║ < N  ║◄────────  tool_use ────► run tools, append      │
│   ║      ║                          to messages, loop      │
│   ╚══════╝◄────────  max_tokens ──► return error           │
│       │                                                    │
│       └──── turn ≥ N ────────────► return MaxTurns error   │
│                                                            │
└────────────────────────────────────────────────────────────┘
```

核心 ~50 行（节选自 [`agents/s01-minimum-loop/loop.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s01-minimum-loop/loop.go)）：

```go
func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
    toolByName := map[string]Tool{}
    schemas := make([]ToolSchema, 0, len(l.Tools))
    for _, t := range l.Tools {
        s := t.Schema()
        toolByName[s.Name] = t
        schemas = append(schemas, s)
    }
    messages := []Message{{
        Role:    "user",
        Content: []ContentBlock{{Type: "text", Text: userPrompt}},
    }}
    for turn := 0; turn < l.MaxTurns; turn++ {
        resp, err := l.Provider.CreateMessage(ctx, CreateMessageRequest{
            Messages: messages,
            Tools:    schemas,
        })
        if err != nil {
            return "", fmt.Errorf("turn %d: %w", turn, err)
        }
        messages = append(messages, Message{Role: "assistant", Content: resp.Content})
        switch resp.StopReason {
        case "end_turn", "stop_sequence":
            return extractText(resp.Content), nil
        case "tool_use":
            results, err := l.runTools(ctx, resp.Content, toolByName, turn)
            if err != nil {
                return "", err
            }
            messages = append(messages, Message{Role: "user", Content: results})
        case "max_tokens":
            return "", fmt.Errorf("hit max_tokens at turn %d", turn)
        default:
            return "", fmt.Errorf("unexpected stop_reason %q", resp.StopReason)
        }
    }
    return "", fmt.Errorf("loop exceeded MaxTurns=%d without end_turn", l.MaxTurns)
}
```

**4 个非显然之处**：

1. **assistant 消息一定要 append，哪怕里面全是 tool_use。** Anthropic 协议要求 assistant 的工具请求作为 history 的一部分参与下一轮调用。漏掉这一步，模型会觉得它"没说过这事"，然后行为奇怪。
2. **tool_result 是 *user* 消息，不是 system 消息。** 这是 Anthropic API 的设计选择。OpenAI 的 chat completion 用 `role: tool`，但 Anthropic 把工具结果当作"用户在向模型展示数据"——所以 role 是 user。
3. **未知工具不 abort，写个 tool_result 让 loop 继续。** 这是 nanobot 也采用的容错策略。"模型问了不存在的工具"是模型的认知问题，不是 loop 的故障——给它机会用别的工具。
4. **MaxTurns 是必须的。** 没有它，模型可以无限地说"我再调一次工具确认下"。production 的 nanobot 默认 50 turns；我们 s01 默认 20，够用。

## What Changed / 与上一节的变化

```diff
+ // s01 是起点。没有"上一节"。
+ // 这一节的代码就是"baseline"——后面 s02..s11 每一节都是这一节的 diff。
```

s01 是 baseline，所以这一节没有 diff。但有一件事值得明说：

**s01 的 `Loop` 这个名字，会在 s11 被回收。** s04 会把 s01 的 Loop 改名为 `Runner`（缩窄它的职责到只管"LLM ↔ tools 工具循环"），然后 s11 用 `Loop` 这个名字实现一个完全不同的 outer state machine。

具体路线：
- s01 `Loop`（这一节）：50 行的 for 循环
- s04 `Runner`：同样的事，但更模块化，不再含 prompt 拼装
- s11 `Loop`（重新启用）：包住 Runner 的 RESTORE→COMPACT→…→DONE 状态机

每一节都是它自己独立的 Go module，所以 s01 的 `Loop` 和 s11 的 `Loop` 在 Go 里是**不同的类型**——package 范围把它们隔开了。这是故意的。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimum-loop

# 不调工具的简单问答
go run . "what is 2+2?"

# verbose 模式，看每一轮的 assistant 消息和 tool 调用
go run . -v "list the .go files in this directory"

# 切个模型
go run . -model claude-haiku-4-5 "echo hi"

# 单元测试（用 fakeProvider，不需要 API key）
go test -v ./...
```

期望输出形态：

```
$ go run . -v "list .go files in this dir"
[s01] model=claude-sonnet-4-6 max_turns=20
[turn 0] assistant: I'll use bash to list .go files.
[turn 0] -> bash map[command:ls *.go]
[turn 0] <- loop.go main.go provider.go tools.go
[turn 1] assistant: 找到了四个 .go 文件：loop.go, main.go, provider.go, tools.go.
找到了四个 .go 文件：loop.go, main.go, provider.go, tools.go.
```

输出文字 LLM 会给不同表达，看的是**形态**——两轮（一轮 tool_use + 一轮 end_turn）、最后输出包含 `loop.go main.go provider.go tools.go`。

## Upstream Source Reading / 上游源码阅读

上游 nanobot 把这一节的 50 行拆成了两个文件：
- `nanobot/agent/runner.py` 的 `AgentRunner.run()`——真正的 LLM ↔ tool 循环（约 250 行）
- `nanobot/agent/loop.py` 的 `AgentLoop.process()`——更外层的状态机（约 600 行）

s01 教 runner 的 conceptual core；s04 显式地碰 runner；s11 才碰外层的 loop 状态机。

完整、注解过的上游片段在仓库根目录的 [`upstream-readings/s01-loop.py`](../../upstream-readings/s01-loop.py) 里。这里只贴最核心的那 30 行（节选）：

```upstream:nanobot/agent/runner.py#L97-L160
class AgentRunner:
    """Run a tool-capable LLM loop without product-layer concerns."""

    def __init__(self, provider: LLMProvider):
        self.provider = provider

    async def run(self, spec: AgentRunSpec) -> AgentRunResult:
        # 1. Bootstrap
        messages = list(spec.initial_messages)
        tools_used: list[str] = []

        # 2. for-loop is the ENTIRE "agent" idea
        for iteration in range(spec.max_iterations):
            response = await self.provider.chat(
                messages=messages,
                tools=spec.tools.get_definitions(),
                model=spec.model,
                temperature=spec.temperature,
                max_tokens=spec.max_tokens,
            )
            messages.append({"role": "assistant",
                             "content": response.content_blocks})

            if not response.tool_calls:
                return AgentRunResult(
                    final_content=response.text,
                    messages=messages,
                    tools_used=tools_used,
                )

            tool_results = []
            for call in response.tool_calls:
                tool = spec.tools.get(call.name)
                if tool is None:
                    tool_results.append(_unknown_tool_result(call))
                    continue
                out = await tool.execute(call.arguments)
                tools_used.append(call.name)
                tool_results.append(_tool_result_block(call.id, out))

            messages.append({"role": "user", "content": tool_results})

        return AgentRunResult(stop_reason="max_iterations", ...)
```

**对照阅读要点**：

- **`provider.chat` vs `provider.CreateMessage`**：上游已经走完了 s03 的 normalization——`response` 是 `LLMResponse`（含 `tool_calls: list[ToolCallRequest]` 和 `text: str`），不是 Anthropic 原始 wire 格式。我们 s01 还没做这一步，所以直接看 `resp.Content` 里的 ContentBlock。
- **`response.content_blocks` 是已经从 LLMResponse 反翻译回来的**。Anthropic 协议要求 assistant 消息保留原始 block 结构（含 thinking、tool_use、text），不能丢。这就是为什么上游要把这层做得很重。
- **`spec.tools` 是 `ToolRegistry`，不是 `[]Tool`。** 这是 s02 的内容——上游一直在用，s01 用 `[]Tool` 是简化。
- **`hook` 在每个关键点会被调用**——`before_iteration`、`on_stream`、`before_execute_tools`、`after_iteration`、`finalize_content`。s09 教这个；s01 完全没有 hook。
- **`AsyncExitStack` 和 `await tool.execute`**——上游全 async；我们 s01 全同步。这两边是相同的逻辑结构，但 Python 的 cooperative multitasking 在 Go 里走 goroutines（s05 才看见这一面）。

**想读更多**：

1. 从 `nanobot/agent/runner.py::AgentRunner.run` 入手，跟着 `self.provider.chat` 进 `nanobot/providers/base.py`（→ s03）。
2. 跟着 `spec.tools.get` 进 `nanobot/agent/tools/registry.py`（→ s02）。
3. 跟着 `hook.before_iteration` 进 `nanobot/agent/hook.py`（→ s09）。
4. 想看 outer state machine？读 `nanobot/agent/loop.py::AgentLoop.process`（→ s11）。

---

**下一节预告**：s02 把 `Tool` 的查找/调度从 `Loop.Run` 里搬出来，封装成一个 `Registry`——加了缓存、稳定排序、register/unregister 接口。`Loop` 从此只管"决定调谁"，不管"怎么找到它"。
