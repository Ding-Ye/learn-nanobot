---
title: "Appendix B · Upstream source-reading map"
chapter: B
slug: appendix-b-upstream-map
est_read_min: 20
---

# Appendix B · Upstream source-reading map

> After learn-nanobot's 11 chapters, the next step is reading [HKUDS/nanobot](https://github.com/HKUDS/nanobot) for real. This appendix is the map.

---

## Recommended reading order

Read in this order; each step is your mini's equivalent reaching its production form. You already know what it's doing — you only need to see how upstream makes it robust.

```
1. nanobot/__main__.py                                  ← CLI entry
2. nanobot/nanobot.py                                   ← SDK facade
3. nanobot/agent/loop.py                                ← matches s11 (longest, ~600 LOC)
   ├ Start: __init__, _state_run, process
   └ Skip: streaming, image-gen, ask_user, subagent

4. nanobot/agent/runner.py                              ← matches s04
   ├ Start: AgentRunSpec, AgentRunResult, run
   └ Skip: retries with heartbeat, length recovery, injection

5. nanobot/providers/base.py                            ← matches s03
   ├ Start: LLMProvider ABC, LLMResponse, ToolCallRequest
   └ Skip: retry helpers (_TRANSIENT_ERROR_MARKERS etc.)

6. nanobot/providers/anthropic_provider.py              ← matches s03
   ├ Start: _build_kwargs, _parse_response
   └ Skip: streaming, thinking blocks, generation_settings

7. nanobot/agent/tools/registry.py                      ← matches s02 (short)
8. nanobot/agent/tools/base.py                          ← matches s01+s02 Tool ABC

9. nanobot/session/manager.py                           ← matches s05
10. nanobot/bus/queue.py or nanobot/bus/events.py        ← matches s05's Bus

11. nanobot/agent/memory.py:1-400                       ← matches s06
12. nanobot/agent/skills.py                             ← matches s07 (medium)
13. nanobot/agent/context.py                            ← matches s08
14. nanobot/agent/hook.py                               ← matches s09 (short)

15. nanobot/agent/memory.py:800-1200                    ← matches s10 (Consolidator)
16. nanobot/agent/autocompact.py                        ← matches s10 (short)
```

After 1-16 you've read the core ~3000 LOC of nanobot. The rest is channel adapters / API server / config — operational, not core.

## Full file → chapter map

| Upstream file | Chapter(s) | Notes |
|---|---|---|
| `nanobot/agent/loop.py:1-120` | s01 | entry skeleton only |
| `nanobot/agent/loop.py` (full) | s11 | the curriculum's biggest mirror |
| `nanobot/agent/tools/registry.py` | s02 | |
| `nanobot/agent/tools/base.py` | s01, s02 | |
| `nanobot/providers/base.py` | s03 | |
| `nanobot/providers/anthropic_provider.py` | s03 | |
| `nanobot/providers/openai_compat_provider.py` | Phase G | not in numbered chapters |
| `nanobot/agent/runner.py` | s04 | |
| `nanobot/session/manager.py` | s05 | |
| `nanobot/bus/queue.py` or `events.py` | s05 | |
| `nanobot/agent/memory.py:1-400` | s06 | file I/O layer |
| `nanobot/agent/memory.py:800-1200` | s10 | Consolidator |
| `nanobot/agent/skills.py` | s07 | |
| `nanobot/agent/context.py` | s08 | |
| `nanobot/agent/hook.py` | s09 | |
| `nanobot/agent/autocompact.py` | s10 | |
| `nanobot/agent/tools/mcp.py` | App. B exercise | optional |
| `nanobot/api/`, `nanobot/channels/`, `nanobot/web/` | App. B exercise | transport layer |
| `nanobot/cli/` | App. B exercise | onboard / agent commands |
| `nanobot/cron/`, `nanobot/heartbeat/` | App. B exercise | operational |
| Dream class in `nanobot/agent/memory.py` | App. A exercise | tier-2 memory rewrite |

## 5 extension exercises

### 1. Add an OpenAI-compatible provider

Implement `Provider` for any OpenAI-compatible endpoint (OpenRouter / DeepSeek / Ollama).

Hint: s03's `LLMResponse` already normalizes finish_reason. What you need is a translator from OpenAI's tool_calls JSON to `[]ToolCall`. **Phase G's `multi-model` doc has done this already** — read it, then implement it yourself for muscle memory.

### 2. Wire a real MCP server

Write a Go MCP client over stdio JSON-RPC and wrap it as a `Tool` so it slots into s02's `Registry`.

Reference: [`nanobot/agent/tools/mcp.py`](https://github.com/HKUDS/nanobot/blob/main/nanobot/agent/tools/mcp.py). For Go: see the [official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) (if it exists) or roll your own JSON-RPC client.

### 3. Implement Dream

App. A describes Dream's purpose: periodically read `history.jsonl`, have the LLM use an `edit_file` tool to rewrite the three markdown files.

Path:
- A `Dream struct` holding `Provider` + `MemoryStore` + `Skills`
- A `(d *Dream) Cycle(ctx)` method: read history → call `Provider.Chat` to extract insights → call again to produce a rewrite plan → call `MemoryStore.WriteMemory`
- A cron-style scheduler that runs it nightly

Reference: the Dream class inside upstream `nanobot/agent/memory.py`.

### 4. Add a Telegram channel

Wrap a Go Telegram library (e.g. `tucnak/telebot`) as a goroutine: receive from Telegram → push to s05's `Bus` → push back.

```
[telegram poller]  ─►  Bus.Send(InboundMessage{...})
                  ◄─    bus.Out() ─►  bot.Send(text)
```

A single new file `agents/sNN-channel-telegram/telegram.go`; no changes elsewhere.

### 5. Streaming responses

s09's `Hook.OnStream` is the seam. Modify s03's `AnthropicProvider` to use the streaming endpoint and call `hook.OnStream(delta)` per token chunk. Then write a `StreamingPrintHook` that flushes delta directly to stdout.

Reference: nanobot's streaming implementation lives in `nanobot/providers/anthropic_provider.py`'s `stream` method + `_LoopHook.on_stream`.

## Closing

If you've finished 11 chapters + these 5 exercises, congratulations — you've independently built a nanobot-equivalent from scratch. Reading upstream source will no longer feel like magic; every line maps to something you've written.

That's the goal of learn-nanobot.
