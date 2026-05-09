---
title: "s04 · Agent runner (inner tool-loop)"
chapter: 4
slug: s04-agent-runner
est_read_min: 22
---

# s04 · Agent runner (inner tool-loop)

> What this teaches: carve s03's `Loop` core into a narrower `Runner` that only drives "call LLM ↔ run tools ↔ repeat" — caller hands in pre-built initial messages. This frees the name `Loop` for s11's outer state machine.

---

## Problem

s03's `Loop` was doing two different things:

1. **Drive the LLM** — `provider.Chat → LLMResponse → run tools → Chat → … → stop`
2. **Build the initial message** — `messages := [{role: user, content: prompt}]` was hard-coded inside the Loop

In a small project that's fine. But upstream nanobot splits these into two layers: the inner one is `AgentRunner` (`runner.py`), the outer one is `AgentLoop` (`loop.py`). `AgentLoop` is the per-turn state machine — RESTORE / COMPACT / BUILD / SAVE / RESPOND. `AgentRunner` only handles the inner LLM ↔ tools loop that lives inside the RUN state.

s11 reintroduces the outer state machine and reuses the name `Loop`. So s04 has to **free that name up** — rename s03's `Loop` to `Runner`, push "construct the initial message" out of the Runner (the caller / s08's ContextBuilder owns that now), and upgrade the return value from a bare `string` to a structured `AgentRunResult`.

While we're carving, we also pick up "length recovery" — a one-shot teaching version of upstream's `_handle_length_recovery`.

## Solution

The new shape:

```go
type AgentRunSpec struct {
    InitialMessages []Message    // caller-built messages (no longer Runner's job)
    Tools           *Registry
    Model           string
    MaxIterations   int
    System          string
}

type AgentRunResult struct {
    FinalContent string         // last assistant text
    Messages     []Message      // full message log (for session persistence)
    ToolsUsed    []string       // de-duplicated tool name list (telemetry)
    Usage        Usage           // token totals across all iterations
    FinishReason string         // "stop" / "length" / "max_iterations" / "error"
}

type Runner struct { Provider Provider }
func NewRunner(p Provider) *Runner
func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error)
```

Three key decisions:

1. **Caller builds InitialMessages.** `Runner.Run` no longer writes `[{user, prompt}]` itself. This is the seam where s08's `ContextBuilder` plugs in: it'll build `[system, memory, skills, history, user]` and hand the whole list to the Runner.
2. **Structured `AgentRunResult` instead of a bare string.** s05's `SessionManager` needs to persist the full message log; s09's hooks need token totals; s11's SAVE state needs the tool-names list. A `string` won't carry that.
3. **Length recovery, but only once.** Upstream allows `_MAX_LENGTH_RECOVERIES = 3`; we ship 1. When `FinishReason == "length"`, we append a "continuation hint" user message and let the next iteration re-prompt. Two `length` reasons in a row → bail.

## How It Works

```ascii-anim frames=2
┌────────────────────────────────────────────────────────────────┐
│                                                                │
│  caller (main / Bus / Loop) builds AgentRunSpec               │
│  ─► spec.InitialMessages = [{user: prompt}]   ← not Runner now │
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
│          if no recoveries left: bail with "length"            │
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

The core ~30 lines (excerpt from [`agents/s04-agent-runner/runner.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s04-agent-runner/runner.go)):

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

**4 non-obvious points**:

1. **`InitialMessages` is slice-copied, not held by reference.** We `append([]Message(nil), spec.InitialMessages...)` to make our own copy. If the Runner held the caller's slice directly, multiple `append` calls inside Run could clobber the caller's backing array — a real concern under s05's Bus, where every session goroutine is calling Run concurrently.
2. **`ToolsUsed` is a de-duplicated, ordered list.** When the LLM calls the same tool many times (typical: `bash` 5 times), upstream's `tools_used` is also dedup'd. We use a `seenTool` map to keep the order of first occurrence.
3. **The length-recovery hint is a `user` message, not `system`.** Anthropic's protocol forbids mid-conversation `system` messages — they only appear at the top level of a ChatReq. A `user` message saying "please continue" is valid and the LLM treats it as a normal turn. Upstream's `build_length_recovery_message()` does the same thing.
4. **`tool_result.ToolUseID` must match the original `tool_use.ID`.** The Provider preserves the ID into `ToolCall.ID`; the Runner writes it back into `ContentBlock.ToolUseID`. Break that pairing and Anthropic returns 422.

## What Changed (vs. s03)

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
-     return resp.Content, nil   // returns just a string
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
+     Messages     []Message     // full log, for session persistence
+     ToolsUsed    []string      // for telemetry / hooks
+     Usage        Usage         // token totals
+     FinishReason string        // stop / length / max_iterations / error
+ }
+ type Runner struct { Provider Provider }
+ func (r *Runner) Run(ctx context.Context, spec AgentRunSpec) (AgentRunResult, error) {
+     messages := append([]Message(nil), spec.InitialMessages...)  // caller pre-built
+     for iter := 0; iter < spec.MaxIterations; iter++ {
+         // same three-way switch (+ "length" gets a recovery branch)
+     }
+ }
```

`main.go` also moved — the caller now writes the first user message itself:

```diff
- final, err := loop.Run(ctx, prompt)
+ result, err := runner.Run(ctx, AgentRunSpec{
+     InitialMessages: []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: prompt}}}},
+     Tools: registry, Model: model, MaxIterations: maxTurns, Verbose: verbose,
+ })
+ fmt.Println(result.FinalContent)
```

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s04-agent-runner

go run . -v "list .go files"

# Tests cover stop / two-tool-calls / max-iterations / partial-failure / length-recovery / provider-error
go test -v ./...

# Trigger a deliberate auth error (401) to see the Runner's wrapped error shape
ANTHROPIC_API_KEY=invalid go run . "test" 2>&1 | head -5
```

Expected output shape:

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

The last line is new in s04 — verbose mode prints `tools_used` + token totals to stderr.

## Upstream Source Reading

Upstream's `nanobot/agent/runner.py` is the canonical reference for this chapter. Full annotated excerpt at [`upstream-readings/s04-agent-runner.py`](../../upstream-readings/s04-agent-runner.py).

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

**5 reading notes**:

- **Upstream's `AgentRunSpec` has 25 fields; ours has 5.** The dropped fields are almost all later-chapter responsibilities: `hook` (s09), `progress_callback` (s09's streaming), `workspace` / `session_key` (s05/s06), `max_tool_result_chars` (s10's truncation), `injection_callback` (advanced sideband injection), `provider_retry_mode` (out of teaching scope). Each chapter introduces one new concept, not 25.
- **`fail_on_tool_error: bool` we don't implement.** Our `Registry.Execute` always wraps tool exceptions as `"tool error: <msg>"` and ships them back as a tool_result block — the LLM decides what to do. Production nanobot can flip `fail_on_tool_error=True` so the Runner aborts on the first failure (good for batch mode). Extension exercise.
- **`concurrent_tools: bool` we don't implement.** When the LLM returns multiple tool_use blocks in one response, upstream can run them in parallel via `asyncio.gather`. We run them sequentially. Go's goroutines would make concurrency cheap, but sequential is easier to read for teaching.
- **`_handle_length_recovery` upstream allows 3 attempts** (`_MAX_LENGTH_RECOVERIES = 3`) and uses `build_length_recovery_message()` to produce a richer hint that re-includes the truncated reply. We allow 1 + a hard-coded string. Production can bump `lengthRecoveriesLeft` to 3 and template the hint.
- **`AgentHookContext` (s09) fires at `before_iteration` / `on_stream` / `before_execute_tools` / `after_iteration`.** Upstream's Runner awaits hook callbacks at each phase. s04 is hookless; s09 adds the callbacks back.

**Read further**:

1. `nanobot/agent/runner.py:200-400` — the full body of `AgentRunner.run()`, especially `_handle_length_recovery`, `_drain_injections`, `_emit_checkpoint`.
2. `nanobot/utils/runtime.py::build_length_recovery_message` — the upstream hint template; it copies the truncated reply back into the prompt as additional context.
3. `nanobot/agent/loop.py::_state_run` — the outer state machine's call site for the Runner. This is the canonical reading for s11.

---

**Next**: s05 wraps the Runner with a per-session goroutine + MessageBus, so multiple sessions can run in parallel while turns within one session stay serialized. The Runner's shape doesn't change — Bus just calls `runner.Run(ctx, spec)` from inside each session's goroutine.
