---
title: "s01 · Minimum agent loop"
chapter: 1
slug: s01-minimum-loop
est_read_min: 25
---

# s01 · Minimum agent loop

> What this teaches: an agent is just an LLM you re-call after every tool. The whole idea is three files and ~320 lines of Go.

---

## Problem

Upstream nanobot is ~5k LOC of Python — 10+ LLM providers, Telegram/Slack/Discord channels, file-system memory, Dream consolidation, MCP integration, a skill system, a TurnState state machine. First contact is overwhelming: which part is "the agent core"? Which parts are scaffolding?

The answer: **the actual agent core is fewer than 50 lines**. Everything else is operational scaffolding around those 50 lines. This chapter is those 50 lines — provider + one tool + a for-loop with a turn cap. After this chapter you have a mental anchor: every later session is "wrap one more concern around this small core".

## Solution

The mental model has four steps:

1. Put the user message into `messages`
2. Call the LLM once
3. If it requests tool calls, execute them and put the results back into `messages` as a *user* message; go to step 2
4. If it says "end_turn", return its final text

Add a max-turns cap to prevent infinite loops, and you're done.

Three key decisions:

1. **`Provider` is an interface, not a struct.** We write `interface { CreateMessage(...) }`. s03 will swap this for a normalized `LLMResponse`; Phase G adds OpenAI-compatible providers — and `loop.go` does not need to change a single line.
2. **`Tool` is also an interface.** s02 moves tool dispatch out of `Loop`; s07 lets Markdown SKILL.md files satisfy this interface too. All of that is possible because s01 made `Tool` an interface from the start.
3. **Tool execution errors are returned as strings, not as Go errors.** This matches what upstream nanobot does — the LLM can reason about stderr; if we propagated errors as Go errors, the loop would abort, and the model would never get the chance to try a different command.

## How It Works

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

The core ~50 lines (excerpt from [`agents/s01-minimum-loop/loop.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s01-minimum-loop/loop.go)):

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

**4 non-obvious points**:

1. **The assistant message must be appended even when it's all `tool_use` blocks.** Anthropic's protocol requires the assistant's tool requests to be part of the conversation history for the next call. Skip this and the model thinks it "didn't say that" — behavior gets weird.
2. **`tool_result` is a *user* message, not a system message.** Anthropic's design choice. OpenAI's chat completion uses `role: tool`, but Anthropic models tool results as "the user is showing me data" — so the role is `user`.
3. **Unknown tool: don't abort, write a `tool_result` and continue.** This is nanobot's strategy too. "Model called a non-existent tool" is a model-side cognition issue, not a loop failure — give it a chance to use a different tool.
4. **`MaxTurns` is mandatory.** Without it, the model can run "let me call one more tool to confirm" forever. Production nanobot defaults to 50 turns; our s01 defaults to 20, which is enough.

## What Changed (vs. s00)

```diff
+ // s01 is the starting point. There is no "previous chapter".
+ // This chapter's code IS the baseline — every later session
+ // (s02..s11) is a diff against this one.
```

s01 is the baseline, so there is no diff. But one thing is worth saying explicitly:

**The name `Loop` from s01 will be reused in s11.** s04 will rename s01's `Loop` to `Runner` (narrowing it to "LLM ↔ tools loop only"), and then s11 will use `Loop` again — for a completely different shape: the outer state machine.

The arc:
- s01 `Loop` (this chapter): the 50-line for-loop
- s04 `Runner`: the same idea, more modular, no prompt-assembly
- s11 `Loop` (re-introduced): the RESTORE→COMPACT→…→DONE state machine that wraps Runner

Each session is its own Go module, so s01's `Loop` and s11's `Loop` are **literally different types** in Go — package-scoping isolates them. That's deliberate.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s01-minimum-loop

# Simple no-tool Q&A
go run . "what is 2+2?"

# Verbose: see every assistant turn and tool call
go run . -v "list the .go files in this directory"

# Different model
go run . -model claude-haiku-4-5 "echo hi"

# Unit tests (no API key required — uses a fakeProvider)
go test -v ./...
```

Expected output shape:

```
$ go run . -v "list .go files in this dir"
[s01] model=claude-sonnet-4-6 max_turns=20
[turn 0] assistant: I'll use bash to list .go files.
[turn 0] -> bash map[command:ls *.go]
[turn 0] <- loop.go main.go provider.go tools.go
[turn 1] assistant: Found four .go files: loop.go, main.go, provider.go, tools.go.
Found four .go files: loop.go, main.go, provider.go, tools.go.
```

The exact text varies — what to match is the **shape**: two turns (one `tool_use` + one `end_turn`), with the final output mentioning `loop.go main.go provider.go tools.go`.

## Upstream Source Reading

Upstream nanobot splits this chapter's 50 lines across two files:
- `nanobot/agent/runner.py` — `AgentRunner.run()`, the actual LLM ↔ tool loop (~250 lines)
- `nanobot/agent/loop.py` — `AgentLoop.process()`, the outer state machine (~600 lines)

s01 teaches the conceptual core of `runner`. s04 explicitly carves out `Runner`. s11 finally tackles the outer `loop`.

The full annotated upstream excerpt is at [`upstream-readings/s01-loop.py`](../../upstream-readings/s01-loop.py). The most relevant 30 lines:

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

**Reading notes**:

- **`provider.chat` vs `provider.CreateMessage`**: upstream has already done s03's normalization — `response` is `LLMResponse` (with `tool_calls: list[ToolCallRequest]` and `text: str`), not the raw Anthropic wire format. Our s01 hasn't done that yet, so we look at `resp.Content` blocks directly.
- **`response.content_blocks` is the round-trip back to Anthropic's block format.** The Anthropic protocol requires the assistant message to keep its original block structure (thinking, tool_use, text). That's why upstream's translation layer is heavy.
- **`spec.tools` is a `ToolRegistry`, not `[]Tool`.** That's s02. Upstream uses Registry from the start; s01 simplifies to a slice.
- **`hook` is invoked at every key point** — `before_iteration`, `on_stream`, `before_execute_tools`, `after_iteration`, `finalize_content`. s09 teaches that; s01 has no hooks.
- **`AsyncExitStack` and `await tool.execute`**: upstream is fully async; our s01 is fully synchronous. Same logical structure, different concurrency models. The Go translation of cooperative multitasking is goroutines (s05 introduces them).

**Read further**:

1. Start at `nanobot/agent/runner.py::AgentRunner.run`, follow `self.provider.chat` into `nanobot/providers/base.py` (→ s03).
2. Follow `spec.tools.get` into `nanobot/agent/tools/registry.py` (→ s02).
3. Follow `hook.before_iteration` into `nanobot/agent/hook.py` (→ s09).
4. For the outer state machine, read `nanobot/agent/loop.py::AgentLoop.process` (→ s11).

---

**Next**: s02 carves the tool lookup/dispatch out of `Loop.Run` into a dedicated `Registry` — with caching, stable ordering, and `Register`/`Unregister`/`Get`/`Execute`. The Loop now just decides who to call; the Registry handles "how to find them".
