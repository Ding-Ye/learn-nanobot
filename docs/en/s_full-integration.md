---
title: "s_full · End-to-end integration"
chapter: full
slug: s_full-integration
est_read_min: 30
---

# s_full · End-to-end integration

> Stitch s01..s11 into one real nanobot call trace. No new code here — this is "the picture you can finally draw after 11 chapters".

---

## Full-stack architecture

```
                  ┌──────────────────────────────────────────────┐
                  │  CLI / Channel (Telegram, Slack, …)          │
                  │     ─ user types message ─                   │
                  └────────────────┬─────────────────────────────┘
                                   │  InboundMessage{SessionKey, UserText}
                                   ▼
              ┌────────────────────────────────────────────────┐
              │   Bus  (s05)  per-session goroutine + chan      │
              │   - serializes within-session                  │
              │   - parallel across sessions                   │
              └────────────────┬───────────────────────────────┘
                               │
                               ▼
                       ┌──────────────────────────────┐
                       │  Loop.Process  (s11)         │
                       │  outer state machine         │
                       │                              │
                       │  RESTORE → COMPACT → COMMAND │
                       │     ↓                        │
                       │  BUILD → RUN → SAVE → RESPOND│
                       │     ↓                        │
                       │  DONE                        │
                       └────┬───┬───┬───┬───┬─────────┘
                            │   │   │   │   │
            ┌───────────────┘   │   │   │   └──────────────┐
            ▼                   ▼   ▼   ▼                  ▼
   SessionManager(s05)  Consolidator(s10) Runner(s04+s09)  MemoryStore(s06)
            │                   │       │       │                  │
            │            (LLM-summarize)│       │            (file I/O)
            │                           │       │
            │                  ┌────────▼───────▼────────┐
            │                  │   Provider (s03)        │
            │                  │ ─ AnthropicProvider ─    │
            │                  │   /v1/messages          │
            │                  └─────────┬───────────────┘
            │                            │
            │                            ▼
            │                   Tool execution
            │                   via Registry (s02)
            │                            │
            │              ┌─────────────┴────────────┐
            │              ▼                          ▼
            │         BashTool(s01-...)          (future: MCP)
            │
            └─►  ContextBuilder (s08) reads:
                   ├ Memory (s06): MEMORY/SOUL/USER.md + history.jsonl
                   └ Skills (s07): every SKILL.md it can find

                  Hook (s09) taps every iteration in Runner.
                  AutoCompact (s10) wakes idle sessions in the background.
```

Every box is annotated with the chapter that built it `(sNN)`. Read s01..s11 in order; each chapter lights up one more box.

## 16-step execution trace

Follow a real request `"search for the latest in AI and summarize"` end-to-end:

| # | Step | Chapter(s) | Code site |
|---|---|---|---|
| 1 | User input → CLI wraps stdin into an `InboundMessage` | (CLI) | `main.go` |
| 2 | `Bus.Send` pushes the message onto the session's channel | s05 | `bus.go::Bus.Send` |
| 3 | Session goroutine receives `msg`, calls `Loop.Process` | s05 + s11 | `bus.go::sessionLoop` |
| 4 | **RESTORE** state: get/create Session via `SessionManager.GetOrCreate(key)`, load existing messages | s05 + s06 | `state_restore.go` |
| 5 | **COMPACT** state: check token budget; if exceeded, call `Consolidator.Consolidate(s)` to compress old turns | s10 | `state_compact.go` |
| 6 | **COMMAND** state: check for slash commands (`/help`, `/clear`); pass through if none | s11 | `state_command.go` |
| 7 | **BUILD** state: `ContextBuilder.BuildSystemPrompt(rt)` assembles AGENTS.md + SOUL.md + USER.md + MEMORY.md + skills + `[Runtime Context]` block | s06 + s07 + s08 | `state_build.go` |
| 8 | **RUN** state enters `Runner.Run`: iteration 0 calls `Provider.Chat(req)` — Anthropic's prompt cache hits because the Registry sort is stable (s02) | s02 + s03 + s04 | `state_run.go` → `runner.go` → `provider_anthropic.go` |
| 9 | LLM returns `LLMResponse{ToolCalls: [{name: "web_search", input: {q: "AI"}}], FinishReason: "tool_calls"}` | s03 | `runner.go` |
| 10 | Runner fires Hook's `BeforeExecuteTools` (telemetry capture), then `Registry.Execute("web_search", {q})` | s09 + s02 | `hook.go::SDKCaptureHook` |
| 11 | Tool actually executes (DuckDuckGo HTTP / Tavily / any Tool impl), returns string | s01 / extension | `bash_tool.go`-style |
| 12 | Runner wraps the tool_result in a user message and calls `Provider.Chat` again — iteration 1; the LLM synthesizes a final answer with the search data | s04 | `runner.go` |
| 13 | LLM returns `LLMResponse{Content: "AI's latest …", FinishReason: "stop"}`; Runner calls Hook.`FinalizeContent` for any final rewrite | s09 | `hook.go` |
| 14 | **SAVE** state: `MemoryStore.AppendHistory` writes both messages to `history.jsonl`; Session.UpdatedAt advances | s06 | `state_save.go` |
| 15 | **RESPOND** state: `tc.FinalContent` flows back through Bus as `OutboundMessage`; CLI prints to stdout | s05 + s11 | `state_respond.go` |
| 16 | (Async) AutoCompact's ticker wakes every 5 minutes; this session is still active so it's skipped; 20 minutes later, the next scan fires `Consolidator.consolidate` to compress old history | s10 | `autocompact.go::Run` |

Every step traces to "where you learned this primitive". This table is the curriculum's diff read backwards.

## Deliberate omissions

| Upstream feature | Why omitted | Where to find it |
|---|---|---|
| MCP tool wrapping | Wraps a Go MCP SDK we'd have to teach first | App. B extension exercise |
| OpenAI-compat router | Phase G addendum adds it | `docs/en/multi-model.md` |
| Channels (Telegram/Slack/Discord/…) | One channel = one transport adapter; we use a single CLI channel | App. B extension exercise |
| Heartbeat / cron / serve | Operational concerns, not agent core | App. B extension exercise |
| Pydantic config validation | Go has no exact equivalent; we use plain structs + `Validate() error` | s11 sidebar |
| `litellm` removal refactor | Python-ecosystem specific | App. A footnote |
| OpenAI Responses API helpers | Provider-specific | Phase G addendum |
| Streaming responses | Educational simplification (hook.OnStream is the seam) | App. B extension exercise |
| Dream second-tier memory compression | Consolidator is the first tier; Dream is a higher-order LLM edit cycle | App. A |

## One-paragraph summary

learn-nanobot is not "lite nanobot" — it's **mental-model nanobot**. After reading s01..s11 you carry a topology of the upstream in your head: which chapter owns what, who depends on whom, why it's sliced this way. Open nanobot's source and every `import` line maps to a learn-nanobot chapter.

Next: two appendices to deepen the model.

- **Appendix A** — the most interesting design choice in nanobot: memory as interpretation, not transcription.
- **Appendix B** — upstream source-reading map, so you can follow the pointers from mini to production.
