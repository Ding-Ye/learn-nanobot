---
title: "s02 · 工具注册表"
chapter: 2
slug: s02-tool-registry
est_read_min: 18
---

# s02 · 工具注册表

> 教什么：把 Tool 的"找谁、按什么序、怎么执行"从 Loop 里搬出来，给一个独立的 `Registry`。Loop 此后只决定"调谁"，找路径、缓存 schema 都是 Registry 的事。

---

## Problem / 问题

s01 的 `Loop.Run` 每一轮都要：(1) 走一遍 `[]Tool` 建一个临时 `map[name]Tool`，(2) 重新构造一份 `[]ToolSchema`。这两步都 O(n)，对一个工具来说无所谓——但 production 的 nanobot 里 builtin + MCP tools 加起来 30+，每轮重建是浪费。

更关键的是**稳定排序**。Anthropic 的 prompt cache 用 prompt 前缀做 hash key——同样的 system + 同样的 tools list（按同样顺序）= cache hit；任何小改 = cache miss。Go 的 map 迭代顺序是随机的，所以如果不排序，每一次的 schema list 在 prompt 里的顺序都不同，prompt cache 永远命中不了。

s01 的 `[]Tool` 写法有这两个潜在问题。s02 修这些。

## Solution / 解决方案

引入一个 `Registry` 类型，把上面两件事一并解决：

1. **缓存 `Definitions()`。** 内部存一份 `cached []ToolSchema`，只在 `Register` / `Unregister` 时清空（设为 nil）。后续每次调 `Definitions()` 直接返回缓存。
2. **稳定排序。** Builtins 按名字排在前，MCP tools（名字以 `mcp_` 开头的，s12 才会有）排后面、也按名字。这套规则和 nanobot 上游 `ToolRegistry.get_definitions()` 一字不差。
3. **typed unknown-tool error。** `Registry.Execute` 在工具不存在时返回 `ErrUnknownTool`，让 Loop 用 `errors.Is` 检测后写一个 `tool_result` 消息——而不是 abort。Tool *运行时* 错误反而被字符串化掉（"command failed" 这种）让模型能 reason about it。

三个关键决策点：

1. **Cache invalidation 只在 mutation 发生**——不是在 `Execute` 时。`Execute` 不改注册表内容，缓存依然有效。
2. **MCP tool 名字用 `mcp_` 前缀做识别**，不是用单独的字段。这是上游约定；我们照搬。
3. **`Registry` 不做参数校验。** 上游的 `prepare_call` 会做 JSON-Schema 校验（"arg 必须是字符串、必须 ≥3 字符" 之类）；我们这一节先省掉，s07+ 等真有需要的时候再加。

## How It Works / 工作原理

```ascii-anim frames=2
┌──────────────────────────────────────────────────────────┐
│                                                          │
│   Loop.Run                                               │
│      │                                                   │
│      ├──── (turn 0)──► r.Definitions() ────► []schema    │
│      │                       (cache hit on turn 1+)      │
│      │                                                   │
│      ├──── tool_use ────► r.Execute(name, input)         │
│      │                       │                           │
│      │                       ├─► r.Get(name)             │
│      │                       │     ok? → t.Execute(...)  │
│      │                       │     not ok? → ErrUnknownTool
│      │                       └─► return out              │
│      │                                                   │
│      └─── result becomes a tool_result ContentBlock ─►   │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

核心 ~30 行（节选自 [`agents/s02-tool-registry/registry.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s02-tool-registry/registry.go)）：

```go
type Registry struct {
    tools  map[string]Tool
    cached []ToolSchema
}

func (r *Registry) Register(t Tool) {
    r.tools[t.Schema().Name] = t
    r.cached = nil // invalidate
}

func (r *Registry) Definitions() []ToolSchema {
    if r.cached != nil { return r.cached }
    var builtins, mcp []ToolSchema
    for _, t := range r.tools {
        s := t.Schema()
        if strings.HasPrefix(s.Name, "mcp_") { mcp = append(mcp, s) }
        else { builtins = append(builtins, s) }
    }
    sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name < builtins[j].Name })
    sort.Slice(mcp, func(i, j int) bool { return mcp[i].Name < mcp[j].Name })
    r.cached = append(builtins, mcp...)
    return r.cached
}

func (r *Registry) Execute(ctx context.Context, name string, input map[string]interface{}) (string, error) {
    t, ok := r.tools[name]
    if !ok { return "", fmt.Errorf("%w: %q", ErrUnknownTool, name) }
    out, err := t.Execute(ctx, input)
    if err != nil { return fmt.Sprintf("tool error: %v", err), nil }
    return out, nil
}
```

**4 个非显然之处**：

1. **`r.cached = nil` 是判 nil，不是判 len==0。** 空 registry 也应该 cache 一个 `[]ToolSchema{}`（cap=0 但非 nil），这样 Definitions() 不会每次都走重建路径。Register/Unregister 时设 nil 触发重建。
2. **builtins + mcp 是两次 append**。第一次 `r.cached = append(builtins, mcp...)` 会先把 mcp 的元素 copy 到 builtins 后面（如果 cap 够）或新分配。这没毛病，但要意识到这是潜在的内存复制点。
3. **`fmt.Errorf("%w", ErrUnknownTool, ...)`** —— 用 `%w` 包装，保留 sentinel 让 Loop 用 `errors.Is(err, ErrUnknownTool)` 检测。如果用 `%v` 就丢信息了。
4. **Tool runtime 错误转成 content（string）返回**。这是 s01 也有的设计。但现在它是 *Registry 的责任*，不是 Loop 的——Loop 不再知道工具 *怎么* 失败的，只看见 string + nil error。

## What Changed / 与上一节的变化

```diff
  type Loop struct {
      Provider Provider
-     Tools    []Tool
+     Tools    *Registry
      MaxTurns int
      Verbose  bool
  }

  func (l *Loop) Run(ctx context.Context, userPrompt string) (string, error) {
-     toolByName := map[string]Tool{}
-     schemas := make([]ToolSchema, 0, len(l.Tools))
-     for _, t := range l.Tools {
-         s := t.Schema()
-         toolByName[s.Name] = t
-         schemas = append(schemas, s)
-     }
+     schemas := l.Tools.Definitions()
      ...
  }

  func (l *Loop) runTools(...) []ContentBlock {
      ...
-     tool, ok := byName[block.Name]
-     if !ok {
-         results = append(results, ContentBlock{... "unknown tool: ..."})
-         continue
-     }
-     out, err := tool.Execute(ctx, block.Input)
-     if err != nil { out = fmt.Sprintf("tool error: %v", err) }
+     out, err := l.Tools.Execute(ctx, block.Name, block.Input)
+     if errors.Is(err, ErrUnknownTool) {
+         out = fmt.Sprintf("unknown tool: %q", block.Name)
+     }
      ...
  }
```

`Loop` 缩短了 ~15 行；那 15 行变成了 `Registry` 里更结构化的代码（缓存、排序、typed error）。这是一次典型的"提取协作者"重构。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s02-tool-registry

# 行为和 s01 一样
go run . -v "list the .go files"

# 测试新功能
go test -v -run TestRegistry ./...

# 全量测试
go test -v ./...
```

期望输出形态（和 s01 几乎一样，只是文件多了）：

```
$ go run . -v "list .go files"
[s02] model=claude-sonnet-4-6 tools=1 max_turns=20
[turn 0] assistant: I'll list the .go files.
[turn 0] -> bash map[command:ls *.go]
[turn 0] <- bash_tool.go loop.go main.go provider.go registry.go tools.go
[turn 1] assistant: Six .go files: ...
Six .go files: ...
```

测试期望全绿，特别注意 `TestRegistry_Definitions_CachedUntilMutation` 显示了缓存命中：连续调用返回的是同一个底层 slice。

## Upstream Source Reading / 上游源码阅读

上游 `nanobot/agent/tools/registry.py` 全文不到 130 行，逻辑和我们的 Go 版几乎一一对应：dict + cache + sort builtins/mcp + typed unknown-tool。

完整注解版在 [`upstream-readings/s02-tool-registry.py`](../../upstream-readings/s02-tool-registry.py)。这里贴最关键的 30 行：

```upstream:nanobot/agent/tools/registry.py#L40-L80
def get_definitions(self) -> list[dict[str, Any]]:
    """Get tool definitions with stable ordering for cache-friendly prompts.

    Built-in tools are sorted first as a stable prefix, then MCP tools are
    sorted and appended.  The result is cached until the next
    register/unregister call.
    """
    if self._cached_definitions is not None:
        return self._cached_definitions

    definitions = [tool.to_schema() for tool in self._tools.values()]
    builtins: list[dict[str, Any]] = []
    mcp_tools: list[dict[str, Any]] = []
    for schema in definitions:
        name = self._schema_name(schema)
        if name.startswith("mcp_"):
            mcp_tools.append(schema)
        else:
            builtins.append(schema)

    builtins.sort(key=self._schema_name)
    mcp_tools.sort(key=self._schema_name)
    self._cached_definitions = builtins + mcp_tools
    return self._cached_definitions
```

**对照阅读要点**：

- **`_schema_name()` 处理两种 schema 形态**——OpenAI 的 nested `{"function": {"name": ...}}` 和 Anthropic 的 flat `{"name": ...}`。我们 s02 还没有 OpenAI provider，所以全部用 flat。s03 之后这层抽象就用得上了。
- **`prepare_call` 做 JSON-Schema 参数校验**——根据每个 Tool 的 `Schema` 类型 cast + validate 参数。我们 s02 故意不做：教这一节的目标是 Registry 的形态，不是参数校验。
- **`_HINT` 后缀**——上游每次错误返回都会附加 `"\n\n[Analyze the error above and try a different approach.]"`，这是 prompt-engineering 帮模型从错误中恢复。我们没做（教学版求清晰）；production 项目应该模仿。
- **dict 迭代顺序在 Python 3.7+ 是 *insertion order***——所以上游的 sort 不是为了"否则随机"，是为了 explicit、跨进程稳定。Go 的 map 迭代是随机的，所以我们更需要 sort。
- **registry 是 sync 不是 thread-safe**——上游和我们都是。多个 goroutine/asyncio task 同时 register 是 race。s05 引入 session 后会看见这一点：每个 session 用自己的 registry 即可，不需要全局加锁。

**想读更多**：
1. `nanobot/agent/tools/registry.py` 完整文件（这个教学版剪掉了 prepare_call 体）
2. `nanobot/agent/tools/base.py` 看 `Tool` ABC + `Schema` 类型，理解 `cast_params` / `validate_params` 怎么工作
3. `nanobot/agent/runner.py::AgentRunner.run` 看 runner 怎么消费 `registry.get_definitions()` 和 `registry.execute()`

---

**下一节预告**：s03 重塑 `Provider` 接口本身——把 Anthropic 的 wire-format `CreateMessageResponse` 翻成规范化的 `LLMResponse`（`Content string` / `ToolCalls []ToolCall` / `FinishReason string`）。这样 OpenAI / Bedrock / 任何后端都能统一输出，Phase G 加多模型就只是"再写一个 Provider 实现"，loop 一行不用改。
