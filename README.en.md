# learn-nanobot

> Build a Go mini-version of [nanobot](https://github.com/HKUDS/nanobot) from scratch — 11 chapters, an end-to-end integration, and two appendices. Each chapter is ~30 minutes: read it, type the code, then read a chunk of the upstream Python source.
>
> 中文：[README.md](./README.md)

## What is this

[HKUDS/nanobot](https://github.com/HKUDS/nanobot) is a ~5k-LOC Python "ultra-lightweight personal AI agent" — 10+ LLM providers, multi-channel chat (Telegram/Discord/Slack/…), file-system memory, MCP integration, Markdown skills. The code is readable, but **how it grew from scratch** is left as an exercise.

`learn-nanobot` fills that gap. Each chapter implements one mechanism in Go (agent loop, tool registry, provider abstraction, session, memory, …) and ends with the corresponding upstream Python source, annotated. After 11 chapters you can read nanobot top-to-bottom.

## Why Go for a Python upstream

Forced translation. `asyncio.Lock` becomes `chan + goroutine`. Pydantic becomes `Validate() error`. Dynamic attributes become explicit structs. Every translation is a deep read of the upstream — you see structure that pure Python reading hides.

Pedagogy borrowed from [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code): mental model before code / 30-60 line core excerpt / diff with the previous chapter / upstream source reading.

## Curriculum

| # | Chapter | Teaches | Status |
|---|---|---|---|
| s01 | [Minimum agent loop](./docs/en/s01-minimum-loop.md) | provider + 1 tool + turn cap | ✅ |
| s02 | [Tool registry](./docs/en/s02-tool-registry.md) | cached + sorted, register/unregister | ✅ |
| s03 | Provider abstraction | `LLMResponse` normalization + retry classification | ⏳ |
| s04 | Agent runner | inner tool-loop carved out | ⏳ |
| s05 | Session + MessageBus | one goroutine per session | ⏳ |
| s06 | Memory store | append-only jsonl + cursor + MEMORY/SOUL/USER.md | ⏳ |
| s07 | Skills loader | YAML frontmatter + requires check | ⏳ |
| s08 | Context builder | bootstrap files + history trim + RuntimeMeta | ⏳ |
| s09 | Hook system | 5 lifecycle points + CompositeHook | ⏳ |
| s10 | Consolidator + AutoCompact | LLM-summarize + TTL background compact | ⏳ |
| s11 | TurnState state machine | RESTORE→COMPACT→COMMAND→BUILD→RUN→SAVE→RESPOND→DONE | ⏳ |
| s_full | End-to-end integration | 16-step execution trace + deliberate-omissions table | ⏳ |
| A | Appendix · Memory as interpretation | append-only → Dream → curated MEMORY.md | ⏳ |
| B | Appendix · Upstream source-reading map | file→chapter map + 5 extension exercises | ⏳ |

Full plan: [`.learn/plan.md`](./.learn/plan.md).

## Run

```bash
# Run s01
cd agents/s01-minimum-loop
export ANTHROPIC_API_KEY=sk-ant-...
go run . "list the .go files in this directory"

# Read the docs (bilingual viewer)
cd web && npm install && npm run dev
# Then open http://localhost:3000
```

Requires Go ≥ 1.23 and Node ≥ 20.

## Layout

```
learn-nanobot/
├── agents/s01-minimum-loop/    # one Go module per chapter; no cross-imports
├── docs/{zh,en}/               # bilingual chapters; CI enforces heading parity
├── upstream-readings/          # annotated real-upstream excerpts
├── web/                        # Next.js doc viewer
├── .github/workflows/          # go.yml / docs.yml / web.yml
└── .learn/                     # generator-left dossier + plan
```

## Credits

- Upstream: [HKUDS/nanobot](https://github.com/HKUDS/nanobot) (MIT License) — all our cross-references point at its source.
- Pedagogy: [shareAI-lab/learn-claude-code](https://github.com/shareAI-lab/learn-claude-code) — chapter structure, bilingual layout, upstream-source-reading pattern all come from there.

## License

MIT — see [`LICENSE`](./LICENSE).
