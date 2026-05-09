---
title: "s02 · Tool registry"
chapter: 2
slug: s02-tool-registry
est_read_min: 18
---

# s02 · Tool registry

> What this teaches: carve "who, in what order, how to dispatch" out of Loop into a dedicated `Registry`. Loop now only decides *who*; finding/caching/ordering becomes the registry's job.

---

## Problem

s01's `Loop.Run` does two O(n) things on every turn: (1) walk `[]Tool` to build an ad-hoc `map[name]Tool`, (2) rebuild `[]ToolSchema`. For one tool that's nothing — but production nanobot has 30+ builtins + MCP tools, and rebuilding every turn is waste.

More importantly: **stable ordering**. Anthropic's prompt cache hashes the prompt prefix — same `system` + same tool list (in same order) = cache hit; any nudge = cache miss. Go's map iteration is randomized, so without sorting, the schema list's order shifts every turn and the prompt cache never hits.

s01's `[]Tool` design has both these latent problems. s02 fixes them.

## Solution

Introduce a `Registry` type that handles both:

1. **Cache `Definitions()`.** Store an internal `cached []ToolSchema`; invalidate (set to nil) only on `Register` / `Unregister`. Subsequent `Definitions()` calls return the cached slice.
2. **Stable ordering.** Built-ins (name-sorted) first; MCP tools (name-prefix `mcp_`, name-sorted) second. Same convention as upstream `ToolRegistry.get_definitions()`.
3. **Typed unknown-tool error.** `Registry.Execute` returns `ErrUnknownTool` when the tool isn't registered — Loop catches that with `errors.Is` and writes a `tool_result` instead of aborting. Tool *runtime* errors instead become string content (so the model can reason about a "command failed").

Three key decisions:

1. **Cache invalidation only on mutation** — not on `Execute`. `Execute` doesn't change registry contents, the cache stays valid.
2. **MCP tools identified by `mcp_` name prefix**, not a separate field. Upstream convention; we copy it.
3. **No parameter validation in `Registry`.** Upstream's `prepare_call` does JSON-Schema validation ("arg must be string, ≥3 chars"); we skip it here and revisit in s07+ when typed args matter.

## How It Works

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

The core ~30 lines (excerpt from [`agents/s02-tool-registry/registry.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s02-tool-registry/registry.go)):

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

**4 non-obvious points**:

1. **The cache check is `cached != nil`, not `len(cached) == 0`.** An empty registry should still return a cached (cap-0 but non-nil) slice; otherwise `Definitions()` rebuilds every call. Setting nil on Register/Unregister triggers rebuild.
2. **`builtins + mcp` is one append op.** `r.cached = append(builtins, mcp...)` either grows builtins in place if cap is sufficient, or allocates fresh. Either way the result is contiguous; just be aware of the allocation point.
3. **`fmt.Errorf("%w", ErrUnknownTool, ...)`** — `%w` wraps so the Loop can use `errors.Is(err, ErrUnknownTool)`. `%v` would lose the type info.
4. **Tool runtime errors get stringified into content.** Same s01 design — but now it's the *Registry's* job, not the Loop's. The Loop never sees *how* a tool failed, only string + nil error.

## What Changed (vs. s01)

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

`Loop` shrinks by ~15 lines; those lines become structured code (cache, sort, typed errors) inside `Registry`. Classic "extract collaborator" refactor.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s02-tool-registry

# Same outward behavior as s01
go run . -v "list the .go files"

# Test the new bits
go test -v -run TestRegistry ./...

# Full test suite
go test -v ./...
```

Expected output shape (almost identical to s01, just more files):

```
$ go run . -v "list .go files"
[s02] model=claude-sonnet-4-6 tools=1 max_turns=20
[turn 0] assistant: I'll list the .go files.
[turn 0] -> bash map[command:ls *.go]
[turn 0] <- bash_tool.go loop.go main.go provider.go registry.go tools.go
[turn 1] assistant: Six .go files: ...
Six .go files: ...
```

All tests should pass; pay particular attention to `TestRegistry_Definitions_CachedUntilMutation` — it verifies the cache hit by checking the underlying slice header is the same on consecutive calls.

## Upstream Source Reading

Upstream's `nanobot/agent/tools/registry.py` is under 130 lines, and its logic maps almost line-for-line to our Go version: dict + cache + sort builtins/mcp + typed unknown-tool.

The full annotated upstream excerpt is at [`upstream-readings/s02-tool-registry.py`](../../upstream-readings/s02-tool-registry.py). Most relevant 30 lines:

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

**Reading notes**:

- **`_schema_name()` handles both schema shapes** — OpenAI's nested `{"function": {"name": ...}}` and Anthropic's flat `{"name": ...}`. We don't have an OpenAI provider yet (s02 still uses s01's Anthropic-native Provider), so all schemas are flat. After s03 we'd need this.
- **`prepare_call` does JSON-Schema parameter validation** — casting + validating against each tool's `Schema` type. We skip this in s02 deliberately; the goal is to teach Registry shape, not param validation.
- **`_HINT` suffix on errors** — upstream appends `"\n\n[Analyze the error above and try a different approach.]"` to every error tool_result. That's prompt-engineering for tool-recovery; primes the model not to repeat the same call. We omit for clarity; production projects should mimic.
- **Python dict iteration is *insertion-order* in 3.7+** — so upstream's sort isn't "otherwise random", it's "explicitly stable across processes". Go's map iteration is genuinely randomized, so we need sort even more.
- **Registry isn't thread-safe** — upstream or ours. Two goroutines/asyncio tasks calling Register concurrently race. After s05 (sessions), each session has its own Registry; no global lock needed.

**Read further**:
1. `nanobot/agent/tools/registry.py` whole file (we trimmed `prepare_call`'s body).
2. `nanobot/agent/tools/base.py` for the `Tool` ABC + `Schema` types — understand how `cast_params` / `validate_params` work.
3. `nanobot/agent/runner.py::AgentRunner.run` — see how the runner consumes `registry.get_definitions()` and `registry.execute()`.

---

**Next**: s03 reshapes `Provider` itself. Anthropic's wire-format `CreateMessageResponse` becomes a normalized `LLMResponse` (`Content string` / `ToolCalls []ToolCall` / `FinishReason string`). Any backend (OpenAI, Bedrock, …) can hit that shape — Phase G's multi-model addendum becomes "just write another Provider implementation"; the loop never changes.
