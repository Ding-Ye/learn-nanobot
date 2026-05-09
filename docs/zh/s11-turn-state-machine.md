---
title: "s11 · 外层状态机"
chapter: 11
slug: s11-turn-state-machine
est_read_min: 36
---

# s11 · 外层状态机

> 教什么：从 s01 到 s10 我们造了 10 个零件——provider、tool、runner、session、bus、memory、skills、context、hooks、consolidator——但 `main.go` 一直在硬接线。上游的答案是真正的外层状态机：`RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE`，转移表是 dict 而不是代码里的 `switch`，per-session lock 由它持有。`Loop` 这个名字回来了，但**类型完全不同**：s01 的 `Loop` 直接包 LLM，s11 的 `Loop` 包了 8 个状态，其中 RUN 状态里再调 `Runner`（也就是 s04 的内层 LLM ↔ 工具循环）。**两层循环、两个职责。** 把这件事看清楚，整个 nanobot 的架构就解锁了。

---

## Problem / 问题

走完 s01 到 s10，我们已经有了 10 个组件：`Provider` 抽象（s03）、`Tool` 接口与 `Registry`（s02）、`Runner` 内层循环（s04）、`Session` 与 `Bus`（s05）、`MemoryStore` 文件持久化（s06）、`SkillLoader` 加载 markdown 技能（s07）、`ContextBuilder` 拼装系统 prompt（s08）、`Hook` 生命周期回调（s09）、`Consolidator` 与 `AutoCompact` 内存压缩（s10）。每一章造一个零件、补一组测试，main.go 把它们手工接起来——读用户输入、查 session、构 prompt、跑 runner、存历史、打印输出。

这个手工接线模式的问题在 s10 之后变得很尖锐。每个新组件让 main.go 多接一段：s06 接了 MemoryStore，s07 加了 SkillLoader，s08 接 ContextBuilder，s09 加了 hook，s10 起了 AutoCompact 的 goroutine。`main.go` 现在 264 行，里面是大量"如果这个 nil 就跳过、否则调那个"的 plumbing。**没有一个地方能让你看到一轮 turn 完整的生命周期长什么样。** 加新功能要在 main.go 多个位置同时改。错误处理散得到处都是。slash 命令？等用户输入到了你才能短路 LLM 调用，但短路逻辑塞在哪？session 锁的获取在哪？前台 compact 触发在哪？

上游对此的答案是把整个 turn 处理流程显式建模成一个状态机：

```
RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE
```

七个非终止状态加一个终止状态。每个状态是一个独立方法。状态之间的转移由一张 `_TRANSITIONS: dict[(TurnState, str), TurnState]` 表声明——是**数据**，不是代码。状态机驱动器是个简单的 `for` 循环：拿当前状态、查 dispatch 表、调对应方法、看返回值决定下一个状态。

为什么这是赢的？三个理由：

1. **可读性。** 想知道一轮 turn 都发生了什么？读 7 个 `_state_X` 方法。每个方法解决一件具体的事，签名都是 `(ctx: TurnContext) -> str`。新读者从上往下扫一遍 `loop.py` 就把整个 agent 的 lifecycle 在脑子里建出来了。

2. **可测试性。** 状态图是数据，可以单独测。`TestTransitions_AllStatesReachableFromRestore` 把表当 graph 走一遍，断言每个状态都从 RESTORE 能走到。如果你将来加一个 `INJECT` 状态忘了接到表里，这个测试当场报错。如果代码用 `switch` 散在十几个地方，这种检查要扒整个调用链才做得了。

3. **关注点分离。** 每个状态拥有它自己的依赖。COMPACT 拥有 Consolidator；BUILD 拥有 ContextBuilder；RUN 拥有 Runner。新功能加在状态边界（"在 BUILD 之后插一个 INJECT 状态接收 sideband 注入"），不是塞进某个 mega-method。

但这里有个让 s01 学 Loop 的读者会犹豫的地方——**`Loop` 这个名字回来了，类型却完全不同**：

- s01.Loop 包的是 `Provider Provider; Tools *Registry; ...`，方法是 `(l *Loop) Step(...)`，作用是"调一次 LLM"。
- s11.Loop 包的是 `Runner *Runner; CtxBuilder *ContextBuilder; Sessions *SessionManager; Memory *MemoryStore; Bus *Bus; Consolidator *Consolidator; Hook Hook`，方法是 `(l *Loop) Process(ctx, msg) (*TurnContext, error)`，作用是"驱动 8 个状态走完一轮 turn"。

它们能共名是因为每个 session 都是独立的 Go module——s01.Loop 和 s11.Loop 在 Go 的包作用域里**字面上是不同的类型**，编译器不会混淆。我们故意复用名字是因为：(a) 上游就是这样命名（`nanobot/agent/loop.py::AgentLoop`）；(b) `Loop` 是这个 conductor 最自然的名字，把它让出去给一个简单 wrapper 只为了避免重名是 unforced error。

记住 diff cue："`Loop` 这个名字回来了。它以前直接包 LLM。现在它包了 8 个状态，其中一个（RUN）通过 `Runner` 包 LLM。从头到尾读 s11 是 learn-nanobot 里最接近读 nanobot 本体的体验。"

## Solution / 解决方案

`turnstate.go` 里 1 个 enum + 1 个 mutable bag + 1 张转移表：

```go
type TurnState int

const (
    StateRestore TurnState = iota
    StateCompact
    StateCommand
    StateBuild
    StateRun
    StateSave
    StateRespond
    StateDone
    StateError
)

func (s TurnState) String() string { /* "RESTORE", "COMPACT", ... */ }

var Transitions = map[TurnState]TurnState{
    StateRestore: StateCompact,
    StateCompact: StateCommand,
    StateCommand: StateBuild,
    StateBuild:   StateRun,
    StateRun:     StateSave,
    StateSave:    StateRespond,
    StateRespond: StateDone,
}

type TurnContext struct {
    Msg              InboundMessage
    SessionKey       string
    State            TurnState
    Session          *Session
    History          []Message
    SystemPrompt     string
    InitialMessages  []Message
    Result           *AgentRunResult
    FinalContent     string
    ToolsUsed        []string
    Generated        []string
    Err              error
}

func (tc *TurnContext) IsTerminal() bool { return tc.State == StateDone || tc.State == StateError }
func (tc *TurnContext) advance() bool { ... }
func (tc *TurnContext) fail(err error) { tc.Err = err; tc.State = StateError }
```

`loop.go` 里 1 个外层状态机：

```go
type Loop struct {
    Runner       *Runner
    Tools        *Registry
    CtxBuilder   *ContextBuilder
    Sessions     *SessionManager
    Memory       *MemoryStore
    Bus          *Bus
    Consolidator *Consolidator
    Hook         Hook
    Model        string
    MaxIter      int
    CompactBudget int
    Verbose      bool
}

func NewLoop(cfg LoopConfig) *Loop
func (l *Loop) Process(ctx context.Context, msg InboundMessage) (*TurnContext, error)
func (l *Loop) dispatch(ctx context.Context, tc *TurnContext) error
```

`Process` 的核心是这个驱动循环：

```go
for !tc.IsTerminal() {
    prev := tc.State
    err := l.dispatch(ctx, tc)
    if err != nil {
        tc.fail(fmt.Errorf("%s: %w", prev, err))
        break
    }
    if tc.State == prev {
        // handler 没改 state，按表前进
        if !tc.advance() { tc.fail(...); break }
    }
    // else: handler 自己改了 state（如 COMMAND 短路到 RESPOND），尊重它
}
```

七个状态文件，每个负责一件事：

- `state_restore.go` — `tc.Session = l.Sessions.GetOrCreate(tc.SessionKey)`。组合 s05。
- `state_compact.go` — 估算 un-consolidated tail，超 budget 就调 `l.Consolidator.Consolidate`。组合 s10。
- `state_command.go` — `/help` 写 helpText 到 `tc.FinalContent`，`tc.State = StateRespond`；`/clear` 清 Session.Messages，同样写 FinalContent 跳到 RESPOND；其他 pass-through。新状态。
- `state_build.go` — 调 `l.CtxBuilder.BuildSystemPrompt` 和 `BuildMessages`，结果写到 `tc.SystemPrompt` 和 `tc.InitialMessages`。组合 s06+s07+s08。
- `state_run.go` — `l.Runner.Run(ctx, spec)`，结果写到 `tc.Result`。组合 s04+s09。
- `state_save.go` — `l.Memory.AppendHistory` 写两条（user + assistant），`tc.Session.AppendAll(post-turn 消息)`。组合 s06。
- `state_respond.go` — 如果 `tc.FinalContent` 已经被 COMMAND 设了就保留，否则从 `tc.Result.FinalContent` 拷过来。新状态。

`main.go` 的 Bus 调用从：

```go
result, err := runner.Run(ctx, spec)  // s10 的形态
```

变成：

```go
tc, err := loop.Process(ctx, msg)     // s11 的形态
finalContent := tc.FinalContent
```

## How It Works / 工作原理

读 `Process` 一遍：

```go
func (l *Loop) Process(ctx context.Context, msg InboundMessage) (*TurnContext, error) {
    tc := &TurnContext{
        Msg:        msg,
        SessionKey: msg.SessionKey,
        State:      StateRestore,
    }
    for !tc.IsTerminal() {
        prev := tc.State
        err := l.dispatch(ctx, tc)
        if err != nil {
            tc.fail(fmt.Errorf("%s: %w", prev, err))
            break
        }
        if tc.State == prev {
            if !tc.advance() {
                tc.fail(fmt.Errorf("no transition from %s", prev))
                break
            }
        }
    }
    if tc.State == StateError {
        return tc, tc.Err
    }
    return tc, nil
}
```

四件事：

1. **种 TurnContext。** Msg、SessionKey、起始 State=Restore。其它字段全是零值，由各 state 填。
2. **跑驱动循环。** 每轮记下当前 state，调 dispatch，再决定下一步。
3. **处理转移。** 如果 dispatch 返回 error → 标 ERROR + 退出。否则如果 handler 自己改了 state（短路情况）→ 尊重；否则查 Transitions 表前进。
4. **返回 tc。** 调用方拿到完整的 TurnContext，包含 FinalContent、Result、ToolsUsed、最后的 State。

七个状态分别对应这些 prior-session 组合：

```
RESTORE  组合 s05 + s06
COMPACT  组合 s10
COMMAND  全新 — 斜杠命令短路
BUILD    组合 s06 + s07 + s08
RUN      组合 s04 + s09
SAVE     组合 s06
RESPOND  全新 — final string assembly
```

整个外层 Loop 的本质就是**编排已有零件**——每个状态最多 50 行代码，因为它真正做的事都委托给前面章节造好的组件。s11 这章的 LOC 大头不是状态本身，而是 turnstate.go 里的注释（解释为什么是 mutable bag、为什么是 map 不是 switch）和 loop.go 的驱动器（解释为什么 handler 可以直接写 tc.State）。

**两层循环、两个职责。** 这是必须建立的心智图。

```
外层循环 (Loop.Process):  for !tc.IsTerminal() { switch tc.State { ... } }
                          每个 turn 走一遍：8 个状态、每个状态调一次

内层循环 (Runner.Run):    for iter < MaxIter { provider.Chat(); execute tools; }
                          每个 LLM 迭代走一遍：调 provider、执行 tool、再调
                          provider，直到 finish_reason="stop"
```

外层管：session 锁、slash 命令短路、历史切片、持久化、错误转移。
内层管：provider 调用、tool 执行、length 恢复、hook 调用。

为什么分两层？因为外层关心的事——锁、文件 I/O、命令分发——是慢、IO-bound、显式的。它们不该塞进内层那个紧凑的 LLM 循环里。每多一个分支，那个 LLM 调用点就多一份理解负担。把它们拆开让两个循环都对自己的职责诚实。

`_TRANSITIONS` 是 map 而不是 switch 的原因：**数据可被检视。** `state_test.go::TestTransitions_AllStatesReachableFromRestore` 把这张表当 graph 走一遍，断言每个状态都从 RESTORE 可达。如果你将来扩展状态机——比如在 BUILD 后面加个 INJECT 状态——这张表是单一真相源，测试会立刻告诉你是否打破了可达性。如果是 switch 块的话这种知识就埋在调用路径里，无从自动检查。

测试侧验证**外层状态机的核心组合性主张**——同样的零件，串到状态机里，行为符合状态图的预期：

```go
// loop_test.go::TestLoop_HappyPath_RestoreToDone
//
// 用 FakeProvider 跑一轮普通用户消息，收集每个 tick 访问的 state，断言
// 顺序是 [RESTORE, COMPACT, COMMAND, BUILD, RUN, SAVE, RESPOND]。
```

```go
// loop_test.go::TestLoop_CommandState_SlashHelp_ShortCircuits
//
// 输入 "/help"。FakeProvider 的 script 是 nil（任何调用都会 "script
// exhausted" 失败）。断言 Process 完整跑完、tc.State == StateDone、
// tc.FinalContent 包含 helpText、fp.Calls() == 0（**LLM 一次都没调**）。
```

```go
// loop_test.go::TestLoop_CompactState_TriggersConsolidator
//
// 预填一个 session 让 un-consolidated tail 估算超过 CompactBudget。
// FakeProvider 提供两个响应（一个给 consolidator 摘要、一个给真正的
// turn）。断言 fp.Calls() == 2，断言 mem.ReadSummary 能读出预期字符串。
```

`TurnContext` 是 mutable bag——每个状态读+写它，不返回新副本。这跟 upstream Python 的 dataclass 模式一一对应。Go 的另一个常见选择是返回 `(*TurnContext, error)`，但那样驱动器要每轮重新拿 pointer，state 之间的 fields 也要重复声明。mutable bag 让 handler 像写普通方法一样写——读 `tc.Session`、写 `tc.SystemPrompt`——驱动器只持有一个 pointer 从头跑到尾。

代价是：handler 中途出错可能让 bag 留在不一致状态。我们靠**事务式 handler** 缓解：每个 handler 要么把它的 outputs 全填完，要么 set State=Error + Err。驱动循环每轮检查 State，所以即便 bag 部分被改了，整个机器还是干净退出。

**错误转移**。任何 state 的 handler 返回 non-nil error 都让驱动器：

```go
tc.fail(fmt.Errorf("%s: %w", prev, err))
```

`tc.Err` 被这条错误包了一层（前缀是出错的 state，比如 "BUILD: stat ..."），`tc.State` 被设为 StateError，外层循环看到 IsTerminal 为 true 就退出。`Process` 返回 `(tc, tc.Err)`，调用方既能看到错误又能看到 tc 的最后状态——对调试很关键。`TestLoop_ErrorInBuild_TransitionsToError` 把 memory 目录从硬盘拽走来触发这条路径，断言 `tc.State == StateError`、`tc.Err != nil`、错误消息里包含 "BUILD" tag。

**slash 命令短路**。`COMMAND` 是唯一一个允许直接改写 `tc.State` 的状态：

```go
case "help":
    tc.FinalContent = helpText
    tc.State = StateRespond  // 跳过 BUILD/RUN/SAVE
    return nil
```

驱动器会 detect `tc.State != prev` 并尊重 handler 的选择（不查 Transitions 表）。这就是 `/help` 在 4 次转移里完成（vs 普通 turn 的 7 次）的机制——并且关键在于**它从来没调过 LLM**。

## What Changed / 与上一节的变化

s10 之后没有新的低层组件——所有 prior-session 的 .go 文件原样搬过来。新增的全是组合层：

- `turnstate.go`（**新**）— `TurnState` enum + `TurnContext` struct + `Transitions` map + 三个小 helpers（`String`、`IsTerminal`、`advance`、`fail`）。
- `loop.go`（**新**）— `Loop`、`LoopConfig`、`NewLoop`、`Process`、`dispatch`。这个 `Loop` **跟 s01 的同名类型不同**：s01.Loop 直接包 LLM；s11.Loop 包 8 个状态，其中一个（RUN）通过 Runner 包 LLM。Go 的包作用域让两个类型字面上互不相同。
- `state_restore.go`、`state_compact.go`、`state_command.go`、`state_build.go`、`state_run.go`、`state_save.go`、`state_respond.go`（**新**）— 七个状态方法，每个一个文件。每个方法都很短（30-100 行），因为重活都委托给 prior-session 组件。
- `loop_test.go`（**新**）— 8 个测试，包括 6 个 spec 要求的：HappyPath、Compact、Command、Run、Save、ErrorInBuild，以及两个补充（多轮幂等、defaults wiring）。
- `state_test.go`（**新**）— 4 个静态图完整性测试：可达性、终止状态没出边、String 全覆盖、advance/fail helper。
- `main.go`（**改写**）— 整体 wiring：构造 `Loop`，调 `loop.Process(ctx, msg)`。verbose 模式下会打印每个状态转移：`[s11] state: RESTORE -> COMPACT`。从输出上你能直接看到状态图被走了一遍。
- 所有其它 .go 文件（**未改**）— provider*、tools.go、bash_tool.go、registry.go、runner.go、bus.go、session.go、skills.go、context.go、hook.go、memory.go、consolidator.go、autocompact.go 全部原样保留。

行数估算（含注释）：

| 文件 | LOC | 角色 |
|---|---|---|
| `turnstate.go` | ~270 | 状态机词汇表（enum + bag + 表 + helpers） |
| `loop.go` | ~265 | 驱动器（`Process` + `dispatch`） |
| `state_*.go` × 7 | ~530 总和 | 状态实现 |
| `loop_test.go` | ~400 | 8 个测试 |
| `state_test.go` | ~150 | 4 个测试 |
| `main.go` | ~230 | CLI wiring |
| **新代码合计** | **~1840** | （prod ~1300 + 测试 ~550） |

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s11-turn-state-machine

# 普通 turn — 走完七个状态
go run . -workspace /tmp/s11-demo -v "list the .go files here"
# [s11] state: START -> RESTORE
# [s11] state: RESTORE -> COMPACT
# [s11] state: COMPACT -> COMMAND
# [s11] state: COMMAND -> BUILD
# [s11] state: BUILD -> RUN
# [iter 0] -> bash {command:ls *.go}
# [iter 0] <- ...
# [s11] state: RUN -> SAVE
# [s11] state: SAVE -> RESPOND
# [s11] state: RESPOND -> DONE

# slash 命令 — 短路到 RESPOND，没调 LLM
go run . -v /help
# [s11] state: RESTORE -> COMPACT
# [s11] state: COMPACT -> COMMAND
# [s11] state: COMMAND -> RESPOND   <-- 跳过 BUILD/RUN/SAVE
# [s11] state: RESPOND -> DONE

# 跑测试
go test -count=1 -race -v ./...
# === RUN   TestLoop_HappyPath_RestoreToDone
# --- PASS: TestLoop_HappyPath_RestoreToDone (0.07s)
# === RUN   TestLoop_CompactState_TriggersConsolidator
# ... 21 个测试全过
# ok  	learn-nanobot/s11	3.5s
```

把 `-v` 拿掉就只看到最终回复——状态机静默运行。把 `-compact-budget 100` 加上让前台 compact 触发（如果 session 里有足够历史）。

挑战：

1. 在 `state_command.go` 里加个 `/status` 命令，回 session 的 Messages 数和 LastConsolidated 值。注意 `/status` 应该跟 `/help` 一样短路到 RESPOND，不调 LLM。
2. 在 BUILD 之后插一个 INJECT 状态：从 stdin 非阻塞读一行（如果有），把它当 user message 加到 `tc.InitialMessages`。这模拟 upstream 的 mid-turn injection。注意你要：(a) 在 turnstate.go 加 `StateInject` enum 值，(b) 在 Transitions 表加两条边（BUILD→INJECT, INJECT→RUN），(c) 写 `state_inject.go`，(d) 跑 `TestTransitions_AllStatesReachableFromRestore`——它现在应该看到 INJECT 出现在已访问集合里。
3. 把 `loopDispatchTrace` 全局换成 `LoopConfig.Trace func(TurnState)`。这要求把每个测试里的赋值改成构造时 inject。是面向生产代码的 refactor。
4. 写一个 `Hook.OnStream` 真正能 stream 的实现——FinalizeContent 时把 token 增量打到 stdout——并把 RUN 的 `Verbose` 跟它接起来。记住 OnStream 是 s09 留的口子，s09-s11 都没真正调它。

## Upstream Source Reading / 上游源码阅读

主要文件：`nanobot/agent/loop.py`（[GitHub](https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/loop.py)），1746 行——repo 里最长的文件，也是 s11 的 canonical reading。

你不需要全读。聚焦 s11 教学点的最小路径只有 200 行：

```
loop.py:187   TurnState 枚举
loop.py:208   TurnContext dataclass
loop.py:258   _TRANSITIONS 表
loop.py:1372  _state_restore
loop.py:1397  _state_compact
loop.py:1402  _state_command
loop.py:1413  _state_build
loop.py:1452  _state_run
loop.py:1475  _state_save
loop.py:1501  _state_respond
```

把每个 `_state_X` 跟 learn-Go 里同名 `state_X.go` 对照读，diff 主要在三处：

1. **media / streaming 全砍。** 上游 BUILD/RUN/SAVE 各处理一堆 image_url / generated_media / on_stream 回调。learn-Go 全部 punt 给 App. B 扩展练习。
2. **CommandRouter 简化。** 上游有动态 command 注册机制（CronTool、SpawnTool 等会注册自己的 `/cron`、`/spawn`）；learn-Go 硬编码 `/help` 和 `/clear`。
3. **runtime checkpoint 没有。** 上游 RESTORE 会从 session 元数据里恢复 "pending user turn"（崩溃恢复用）。learn-Go 假设每个 turn 是干净的。

完整阅读笔记见 `upstream-readings/s11-turn-state-machine.py`——里面给了三个 _state_X 方法的 simplified 版加 reading map。

参考 LOC：

```
nanobot/agent/loop.py:1-77       imports + 常量。略读。
nanobot/agent/loop.py:79-185     _LoopHook (内部 hook for main loop)。略读。
nanobot/agent/loop.py:187-238    TurnState + StateTraceEntry + TurnContext。**仔细读**。
nanobot/agent/loop.py:240-450    AgentLoop.__init__ + tool registry plumbing。略读。
nanobot/agent/loop.py:450-1370   provider snapshot, hook plumbing, run_agent_loop。
                                 仔细读 run_agent_loop——它对应 s04 Runner.Run。
nanobot/agent/loop.py:1372-1512  七个 _state_X 方法 + _TRANSITIONS。**这是 s11**。
nanobot/agent/loop.py:1513-1700  message 持久化助手、sanitize、outbound 组装。
                                 深度 dive 时再读。
nanobot/agent/loop.py:1700-1746  process_direct, process_message, lock 获取。
                                 理解 bus 怎么驱动状态机时读。
```

下一节：s_full 整合章节——读 16 步端到端 trace，看每一步如何引用 s01-s11。
