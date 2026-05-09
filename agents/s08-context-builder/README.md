## s08 · context-builder

> 把 s06 的 MemoryStore + s07 的 SkillLoader + s05 的 Session 凑到一起，组装出 system prompt 和给 Runner 的 `[]Message`。`<think>` 块在唯一的 `sanitize` 边界被剥掉。
> Compose s06's MemoryStore, s07's SkillLoader, and s05's Session into a system prompt and a `[]Message` for the Runner. `<think>` blocks are stripped at the *single* sanitize boundary.

### Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -workspace /tmp/agent-demo -skills weather,echo -v "what's the weather in Tokyo"
# [s08] bootstrap files present: 0/4 ([AGENTS.md SOUL.md USER.md TOOLS.md])
# [s08] loaded 2 skill(s) from workspace="/tmp/agent-demo/skills" builtin=".../testdata/skills"
#        - echo
#        - weather
# [s08] skills requested for system prompt: [weather echo]
# [s08] system prompt: 612 chars; messages: 1
# ...
```

### Files / 文件

| File | What's new vs s07 |
|---|---|
| `provider*.go`, `tools.go`, `bash_tool.go`, `registry.go`, `runner.go`, `bus.go`, `session.go`, `memory.go`, `skills.go` | unchanged — carried over verbatim |
| `context.go` | **new** — `ContextBuilder`, `BuildSpec`, `RuntimeMeta`, `sanitize`, `trimHistoryByTokens` |
| `context_test.go` | 7 tests covering bootstrap files, runtime block, message-count trim, token-budget trim, sanitize idempotence, no-skills path |
| `main.go` | wires `ContextBuilder` between session lookup and `Runner.Run`; prints loaded skill + bootstrap counts in `-v` |

### What ContextBuilder produces / 产出

**System prompt** (joined by `\n\n---\n\n`):

1. Bootstrap files: `## AGENTS.md` / `## SOUL.md` / `## USER.md` / `## TOOLS.md` (only those that exist)
2. `# Memory` section reading from `MEMORY.md`
3. `# Skills` section from `SkillLoader.LoadForContext(SkillsRequested)`
4. `[Runtime Context — metadata only]` block — Now / Channel / ChatID / Sender, fenced

**Messages** (returned to Runner):

- prior history from Session, trimmed to `MaxHistoryMessages` then to `MaxSystemTokens` (using `len(s)/4`)
- one trailing user message containing `UserText`

The system prompt is passed via `RunSpec.System` (separate from `InitialMessages`) — this seam was created in s04 and finally lights up in s08.

### The single sanitize boundary / 唯一的清洗边界

Anti-pattern #4 from `research-notes.md`: `<think>...</think>` blocks must be stripped at exactly *one* place. We strip in `BuildMessages`. The Runner does not strip. The Hook does not strip (s09). Persistence does not strip (`MemoryStore.AppendHistory` writes the already-sanitized content).

Apply twice → double-strip can lose intermediate text.
Apply inconsistently → reasoning leaks into history and ends up in the next turn's prompt.

### Token budget caveat / 令牌预算说明

`approximateTokens` uses `len(s)/4`. This is the placeholder s10 replaces with a real tiktoken-style estimator. For English text it's within ~10-20 % of the true count; for code or non-Latin text it's noticeably off. The 4-char-per-token rule is good enough to pin down the *shape* of the trim logic — s10 demonstrates how to slot in a better counter.

### Test / 测试

```bash
go test -count=1 -race ./...
```

### Teaching points / 教学要点

1. **The system prompt is the output of a function**, not a string literal. It reads 4+ files, loads N skills, trims history to a budget, adds runtime metadata, and joins everything with `\n\n---\n\n`.
2. **`RunSpec.System` is a different parameter from `RunSpec.InitialMessages`.** Anthropic's protocol treats them separately; we already split them in s04 — s08 is when the seam finally sees use.
3. **Two history caps, applied in order.** First `MaxHistoryMessages` (a strict tail of N), then `MaxSystemTokens` (drop oldest when budget is tight). Both have to be > 0 to take effect.
4. **`<think>` stripping is single-boundary.** `sanitize` lives in `context.go` and is called exactly once per text block. Don't add another call site — if the contract is broken, double-stripping or leaks become hard-to-debug bugs.
5. **`RuntimeMeta.Now` is injected, not read.** Tests want determinism. Production injects `time.Now()`. The same code path serves both.

### Next / 下一节

s09 makes the Runner observable via `Hook` callbacks: `BeforeIter` / `OnStream` / `BeforeExecuteTools` / `AfterIter` / `FinalizeContent`. After that, every interesting moment in a turn has a tap point.
