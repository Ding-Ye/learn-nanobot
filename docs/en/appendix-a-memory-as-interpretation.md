---
title: "Appendix A · Memory as interpretation"
chapter: A
slug: appendix-a-memory-as-interpretation
est_read_min: 18
---

# Appendix A · Memory as interpretation, not transcription

> The most interesting — and most easily misread — design in nanobot. In one sentence: **the agent's memory has two tiers; the upper tier isn't a log, it's a README the LLM wrote**.

---

## What you see vs. what's designed

After s06 you know nanobot's workspace contains four files:

```
~/.learn-nanobot/agents/default/
├── history.jsonl    ← one turn per line, append-only, never deleted
├── MEMORY.md        ← markdown about "facts"
├── SOUL.md          ← markdown about "who the agent is"
└── USER.md          ← markdown about "user preferences"
```

By name alone this looks like an unremarkable "log + three settings files". But:

1. **`history.jsonl` is written by code; the three `.md` files are written by an LLM** — the latter aren't user-edited; the agent reflects on its own history and rewrites them periodically.
2. **The three markdown files can be rewritten by the LLM.** That's exactly what upstream nanobot's *Dream* phase does: the agent reads its own history and uses an `edit_file` tool to rewrite `MEMORY.md`.
3. **It's *interpretation*, not *transcription*.** `history.jsonl` is the fact record; the three markdown files are *the agent's understanding of its own experience*.

## A three-layer mental model

```
                    HISTORY                        MEMORY
                 (transcript layer)             (interpretation layer)
                                  ┌──────────►
                                  │
            history.jsonl ────────┤              MEMORY.md
            (append-only,         │              (facts + observations)
             never lost)          │
                                  ├──────────►
                                  │              SOUL.md
            ↑ Consolidator (s10)  │              (agent persona)
            ↑   folds into one    │
            ↑   paragraph         │              USER.md
                                  │              (user preferences)
            ↑ Dream (upstream
            ↑   only, not in mini)
            ↑ Periodically reads
            ↑   history, uses
            ↑   edit_file to
            ↑   rewrite the .md
            ↑   files
```

Tier 1: **append-only history**. The objective transcript of what the agent experienced. Never deleted, never edited. Audit trail.

Tier 2: **curated markdown**. The README the LLM writes for its future self. Can be rewritten — because new experiences make old understandings obsolete. This tier is *interpretation*.

In between, two LLM operations bridge them:

- **Consolidator** (you wrote it in s10) — when history grows too long, fold the earliest stretch into a summary, but *the summary is still factual*; just shorter.
- **Dream** (upstream has it; learn-nanobot doesn't) — periodically reads history, has the LLM use the `edit_file` tool to *rewrite* `MEMORY.md` / `SOUL.md` / `USER.md` — i.e., re-interpret its own experience.

## Why this beats two obvious alternatives

### Alternative 1: pure append-only log

Append everything to one ever-growing log. Pro: never lose information. Con: the context window blows — every turn loads all of history. The surface fix is RAG (vector retrieval), but agent memory isn't only "find info"; much of it is "know who I am".

### Alternative 2: pure in-place markdown

Sync everything directly into a single `MEMORY.md`; load on demand. Pro: compact context. Con: no audit trail — once the model edits something wrong, no way back; and "facts" and "understanding" are conflated, so you can't tell "what just happened" from "what the agent thinks about it".

### nanobot's choice

**Keep both layers.** Bottom layer immutable; top layer mutable. LLM compresses + rewrites in between. The result:

- Audit trail lives in `history.jsonl`
- The prompt-participating thing is markdown (compact)
- The agent can *self-correct* — instead of being permanently bound by an immutable history

This is *memory* in the engineering sense, not storage in the database sense.

## Why we didn't ship Dream

learn-nanobot stops at Consolidator because:

1. Consolidator already demonstrates the "two-tier + LLM-compression" pattern fully.
2. Dream is engineering-wise a more aggressive Consolidator (rewriting markdown vs. appending summaries); the mental model is unchanged.
3. By LOC, Dream is ~300 lines — too small for its own chapter, too bloating to merge into s10.

App. B lists Dream as "Extension exercise #3" — after finishing learn-nanobot, you should be able to write Dream from this appendix's description alone, without further hand-holding.

## Cross-chapter references

- **s06**: the physical layout of the four files
- **s10**: the Consolidator implementation, tier-1 compression
- **s_full** step 16: AutoCompact wakes this flow in the background
- **App. B**: upstream `nanobot/agent/memory.py`'s Dream class is what this appendix describes in code

## One-sentence takeaway

An agent is stateful — across turns, across sessions. But this statefulness is *non-monotonic* — the agent is allowed to *revise* its understanding of the past. That's the fundamental difference between an agent and a chatbot.
