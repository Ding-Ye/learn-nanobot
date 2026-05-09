## s07 · skills-loader

> 给 agent 加 SkillLoader：走 `<workspace>/skills/` + builtin 两个目录、解析 YAML frontmatter、用 `requires.bins` / `requires.env` 做实时检查、按需拼成 system prompt 片段。技能不是代码，是 Markdown——LLM 自己读自己的 runbook。
> Add a `SkillLoader` to the agent: walks `<workspace>/skills/` and a builtin dir, parses YAML frontmatter, filters with `requires.bins` / `requires.env`, and renders Markdown sections on demand. Skills aren't code — they're Markdown the LLM reads as its own runbook.

### Run / 运行

```bash
export ANTHROPIC_API_KEY=sk-ant-...
go run . -workspace /tmp/agent-demo -v "list .go files"
# [s07] loaded 2 skill(s) from workspace="/tmp/agent-demo/skills" builtin=".../testdata/skills"
#        - echo (ok) — Echo a string back as-is
#        - weather (ok) — Get current weather for a city
```

### Files / 文件

| File | What's new vs s06 |
|---|---|
| `provider*.go`, `tools.go`, `bash_tool.go`, `registry.go`, `runner.go`, `bus.go`, `session.go`, `memory.go` | unchanged — carried over verbatim |
| `skills.go` | **new** — `SkillLoader`, `Skill`, `SkillFrontmatter`, `parseSkill`, `checkRequirements` |
| `skills_test.go` | 7 tests covering frontmatter parsing, malformed skip, missing-bin filter, LoadForContext, disabled list, workspace>builtin precedence, `enabled: false` opt-out |
| `testdata/skills/{weather,echo}/SKILL.md` | example skills used by the verbose demo |
| `main.go` | adds `-skills-dir`, `-builtin-skills`, `-disabled-skills`, `-v` flags; loads skills at startup |
| `go.mod` | adds `gopkg.in/yaml.v3 v3.0.1` |

### Skill format / 技能格式

```markdown
---
name: weather
description: Get current weather for a city
requires:
  bins: [curl]
  env: [WEATHER_API_KEY]
enabled: true        # optional; absent => true
---
The Markdown body goes here. The LLM reads this verbatim
as part of the system prompt and decides what to do.
```

Three filter layers, in order:

1. **`-disabled-skills` flag** — operator config; unconditional skip.
2. **`enabled: false` in frontmatter** — author opt-out; skill disappears.
3. **`requires.bins` / `requires.env`** — runtime guard; skill remains in `List()` but `Available=false` and `LoadForContext` skips it. (s08 may surface unavailable skills in a separate "not callable right now" section.)

Workspace skills override builtin skills with the same name, so users can customize defaults.

### Test / 测试

```bash
go test -count=1 -race ./...
```

### Teaching points / 教学要点

1. **A skill is Markdown, not code.** `SkillLoader.LoadForContext` drops the body verbatim into the system prompt. The LLM, not Go, decides what to do with it.
2. **YAML frontmatter is the seam** between metadata (name, description, requires) and content (Markdown body). Use `gopkg.in/yaml.v3` — there's no stdlib YAML.
3. **`requires` is a real-time guard.** `exec.LookPath("curl")` runs every time `List()` is called, so a skill becomes available the moment its dependency is installed (no daemon restart needed).
4. **`enabled: false` is opt-out, not opt-in.** Absence defaults to enabled — the common case is "ship the skill, let it run". Authors only need to set `enabled` when they're drafting / disabling something temporarily.
5. **One bad SKILL.md never breaks the whole loader.** Malformed YAML is silently skipped; the rest of the directory loads normally. This is upstream's invariant too.

### Next / 下一节

s08's `ContextBuilder` finally puts these pieces together: it reads `MemoryStore` (s06) for SOUL/USER/MEMORY, calls `SkillLoader.LoadForContext` (this chapter) for the requested skills, and assembles the full system prompt + history that `Runner.Run` (s04) consumes. After s08, the agent is a cohesive whole.
