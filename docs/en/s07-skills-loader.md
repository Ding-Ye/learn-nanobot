---
title: "s07 · Skills loader"
chapter: 7
slug: s07-skills-loader
est_read_min: 22
---

# s07 · Skills loader

> What this teaches: add a `SkillLoader` to the agent. It walks `<workspace>/skills/` and a builtin directory, parses YAML frontmatter, runs a real-time availability check via `requires.bins` / `requires.env`, and renders selected skills as Markdown sections that s08's ContextBuilder will splice into the system prompt. **A skill is Markdown, not code** — the LLM reads its own runbook.

---

## Problem

Through s06 the agent has: provider abstraction, tool registry, inner LLM loop, Session, MessageBus, file-system memory. But its *behavior* is determined by exactly two things:

- **A static system prompt** (a string literal since s01),
- **A runtime tool list** (whatever's `Register`-ed into `Registry`).

If you want an agent that "knows how to check the weather" — you don't have to write a Go `weather_api` tool. The lighter nanobot path is to write a `SKILL.md`:

```markdown
---
name: weather
requires:
  bins: [curl]
---
To get weather, run `curl wttr.in/<city>?format=3`. The result is human-readable.
```

Stuff this Markdown into the system prompt and the LLM sees that it should call the existing `bash` tool with `curl wttr.in/...` — no new Go code needed. Skills are the agent's runbook: which CLI to invoke, which API to call, which cadence to follow.

That introduces 4 engineering problems:

1. **Where to read from?** User-authored skills should be separate from builtin ones, but precedence has to be explicit (user wins).
2. **How to parse?** YAML frontmatter + free-form body — need a splitter.
3. **How to gate availability?** A `weather` skill that needs `curl` shouldn't be inserted into the prompt when `curl` isn't on PATH — the LLM would call a command that fails.
4. **How to disable?** Operators need `-disabled-skills weather`; authors need `enabled: false` in frontmatter. Two separate paths.

`SkillLoader` packages all four into one type.

## Solution

A new file `skills.go` introduces 4 types and 5 functions:

```go
// skills.go
type SkillRequires struct {
    Bins []string `yaml:"bins,omitempty"`
    Env  []string `yaml:"env,omitempty"`
}

type SkillFrontmatter struct {
    Name        string        `yaml:"name"`
    Description string        `yaml:"description"`
    Requires    SkillRequires `yaml:"requires,omitempty"`
    Enabled     *bool         `yaml:"enabled,omitempty"` // pointer = three-state
}

type Skill struct {
    Name              string
    Description       string
    Body              string  // Markdown body with frontmatter stripped
    Path              string
    Available         bool
    UnavailableReason string
}

type SkillLoader struct { workspaceRoot, builtinRoot string; disabled map[string]bool }

func NewSkillLoader(workspace, builtin string, disabled []string) *SkillLoader
func (s *SkillLoader) List() []Skill
func (s *SkillLoader) Load(name string) (*Skill, bool)
func (s *SkillLoader) LoadForContext(names []string) string
func parseSkill(path string) (*parsedSkill, error)        // internal
func checkRequirements(req SkillRequires) (bool, string)  // internal
```

Three filter layers, in order:

| Layer | Decided by | Behavior |
|---|---|---|
| `disabled []string` | Operator (CLI flag) | Removed from `List()` |
| `enabled: false` (frontmatter) | Skill author | Also removed from `List()` |
| `requires.bins` / `requires.env` | Runtime | Stays in `List()` but `Available=false`; `LoadForContext` skips it |

Why three separate layers and not one? They have different semantics:

- The first two are **configuration** ("this machine / this repo doesn't want this skill to appear right now").
- The third is **availability** ("the skill exists but can't run right now"). s08 may render unavailable skills in a "you have these but can't use them" section — that requires the layer-3 skills to remain in `List()`.

`Enabled *bool` is a pointer, not a bool. Three states in one field: `nil` (default → enabled), explicit `false` (opt-out), explicit `true` (no-op).

Both the workspace and builtin roots are walked, and **the workspace version wins on name collision** — that's the rule that lets users override a builtin skill (e.g. ship their own `weather` that uses a paid API instead of the free wttr.in).

## How It Works

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  main.go                                                         │
│    skills := NewSkillLoader(workspaceSkills, builtinSkills, ds)  │
│    if verbose { for sk := range skills.List() { print(sk) } }    │
│    // s07 doesn't yet inject skills into the system prompt —    │
│    // that's s08's ContextBuilder.                               │
│                                                                  │
│  SkillLoader.List():                                             │
│    walk(workspaceRoot)  →  parseSkill(.../weather/SKILL.md)      │
│    walk(builtinRoot)    →  parseSkill(.../echo/SKILL.md)         │
│    dedupe (workspace wins)                                       │
│    drop disabled / enabled:false                                 │
│    sort.Slice by Name                                            │
│                                                                  │
│  parseSkill(path):                                               │
│    raw := os.ReadFile(path)                                      │
│    if startswith "---\n":                                        │
│        yamlText, body := split(raw, "\n---")                     │
│        yaml.Unmarshal(yamlText, &fm)                             │
│    available, reason := checkRequirements(fm.Requires)           │
│    return Skill{Name, Description, Body, Path, Available, …}     │
│                                                                  │
│  checkRequirements(req):                                         │
│    for bin in req.Bins  : if !exec.LookPath(bin)  : missing+= "CLI: "+bin
│    for env in req.Env   : if os.Getenv(env)==""   : missing+= "ENV: "+env
│    return len(missing)==0, strings.Join(missing, ", ")           │
│                                                                  │
│  ┌──────────── Markdown ≠ Code ───────────────────────────────┐  │
│  │                                                            │  │
│  │   How a SKILL.md ends up in the system prompt:             │  │
│  │                                                            │  │
│  │     ### Skill: weather                                     │  │
│  │     To get weather, run `curl wttr.in/<city>?format=3`...  │  │
│  │     ---                                                    │  │
│  │     ### Skill: echo                                        │  │
│  │     The user wants you to echo a phrase verbatim. Repeat.  │  │
│  │                                                            │  │
│  │   The LLM reads its own runbook — Go never parses body.    │  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

The core ~30 lines (excerpt from [`agents/s07-skills-loader/skills.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s07-skills-loader/skills.go)):

```go
func parseSkill(path string) (*parsedSkill, error) {
    raw, err := os.ReadFile(path)
    if err != nil { return nil, err }
    body := strings.ReplaceAll(string(raw), "\r\n", "\n")

    fm := SkillFrontmatter{}
    rest := body

    if strings.HasPrefix(body, "---\n") {
        closeIdx := strings.Index(body[4:], "\n---")
        if closeIdx < 0 {
            return nil, errors.New("opening --- has no matching closing ---")
        }
        yamlText := body[4 : 4+closeIdx]
        afterClose := 4 + closeIdx + len("\n---")
        if afterClose < len(body) && body[afterClose] == '\n' {
            afterClose++
        }
        rest = body[afterClose:]
        if err := yaml.Unmarshal([]byte(yamlText), &fm); err != nil {
            return nil, fmt.Errorf("yaml: %w", err)
        }
    }

    name := fm.Name
    if name == "" { name = filepath.Base(filepath.Dir(path)) }

    if fm.Enabled != nil && !*fm.Enabled {
        return &parsedSkill{frontmatterDisabled: true,
            skill: Skill{Name: name, Path: path, Body: rest, Description: fm.Description}}, nil
    }
    available, reason := checkRequirements(fm.Requires)
    return &parsedSkill{skill: Skill{
        Name: name, Description: fm.Description,
        Body: strings.TrimLeft(rest, "\n"),
        Path: path, Available: available, UnavailableReason: reason,
    }}, nil
}

func checkRequirements(req SkillRequires) (bool, string) {
    var missing []string
    for _, bin := range req.Bins {
        if _, err := exec.LookPath(bin); err != nil { missing = append(missing, "CLI: "+bin) }
    }
    for _, env := range req.Env {
        if os.Getenv(env) == "" { missing = append(missing, "ENV: "+env) }
    }
    if len(missing) == 0 { return true, "" }
    return false, strings.Join(missing, ", ")
}
```

**5 non-obvious points**:

1. **YAML frontmatter is the only seam between metadata and content.** The opening 4 lines of `---/.../---` are for `SkillLoader`; everything below is for the LLM. **Go never parses the body** — it's just a string. The LLM does the interpretation when it reads the prompt.
2. **`requires` is a runtime guard, not a declarative manifest.** Python's `pyproject.toml`-style "declare your dependencies once" is set-and-forget; `requires.bins` runs `exec.LookPath` on every `List()` call — so after `brew install curl` the weather skill becomes available immediately, no agent restart needed.
3. **`enabled` is `*bool`, not `bool`.** Three states: `nil` (default → enabled), explicit `false` (opt-out), explicit `true` (explicit enabled, no-op). YAML expresses this naturally — authors omit the field most of the time and only set it when drafting/disabling.
4. **A skill with `Available=false` still appears in `List()`.** It just isn't rendered by `LoadForContext`. This gives s08 the freedom to surface unavailable skills in a separate "you have these but can't call them right now" section — useful so the LLM can ask the user to install `curl`. Filtering at layer 3 happens at render time, not list time.
5. **One bad SKILL.md never breaks the loader.** When `parseSkill` fails the loader silently skips that skill; the rest of the directory loads normally. This is upstream's invariant — and the `Parse_MalformedFrontmatter_SkippedWithWarning` test enforces it.

## What Changed (vs. s06)

```diff
  // s06/main.go: just MemoryStore
- mem, _ := NewMemoryStore(root)
- ...
- bus.Send(...); out := <-bus.Out()
- mem.AppendHistory(...)

  // s07/main.go: adds a SkillLoader (not yet wired into the system prompt — s08's job)
+ skillsRoot := filepath.Join(root, "skills")
+ builtinRoot := filepath.Join(repoRoot(), "testdata", "skills")
+ skills := NewSkillLoader(skillsRoot, builtinRoot, disabled)
+ if verbose { for _, sk := range skills.List() { fmt.Printf("  - %s\n", sk.Name) } }
  ...
  bus.Send(...)
```

New types (the s07 spine):

```go
+ type SkillRequires struct { Bins, Env []string }
+ type SkillFrontmatter struct { Name, Description string; Requires SkillRequires; Enabled *bool }
+ type Skill struct { Name, Description, Body, Path string; Available bool; UnavailableReason string }
+ type SkillLoader struct { workspaceRoot, builtinRoot string; disabled map[string]bool }
```

New dependency:

```go
require gopkg.in/yaml.v3 v3.0.1
```

`MemoryStore` / `SessionManager` / `Bus` / `Runner` / `Provider` / `Tool` / `Registry` are all unchanged — not a single line modified. That's s07's payoff: it's **purely additive**, no refactor of earlier scaffolding.

## Try It

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s07-skills-loader

# Run once with verbose to see which skills loaded
go run . -workspace /tmp/agent-demo -v "list .go files"
# [s07] loaded 2 skill(s) from workspace=".../skills" builtin=".../testdata/skills"
#        - echo (ok) — Echo a string back as-is
#        - weather (ok) — Get current weather for a city
```

Try the disabled list:

```bash
go run . -workspace /tmp/agent-demo -disabled-skills weather -v "test"
# [s07] loaded 1 skill(s)
#        - echo (ok) — Echo a string back as-is
```

Try workspace-overrides-builtin:

```bash
mkdir -p /tmp/agent-demo/skills/weather
cat > /tmp/agent-demo/skills/weather/SKILL.md <<'EOF'
---
name: weather
description: my paid weather endpoint
requires:
  bins: [curl]
---
Use https://my-weather.example.com with the X-API-Key header.
EOF

go run . -workspace /tmp/agent-demo -v "test"
# - weather (ok) — my paid weather endpoint   ← workspace version won
```

Try missing-bin:

```bash
mkdir -p /tmp/agent-demo/skills/exotic
cat > /tmp/agent-demo/skills/exotic/SKILL.md <<'EOF'
---
name: exotic
description: needs a CLI that doesn't exist
requires:
  bins: [definitely-not-a-real-bin]
---
EOF

go run . -workspace /tmp/agent-demo -v "test"
# - exotic (unavailable: CLI: definitely-not-a-real-bin) — needs a CLI that doesn't exist
```

Run the tests (race detector required):

```bash
go test -count=1 -race -v ./...
```

Expected: 7 tests pass — `Parse_Frontmatter_Standard` / `Parse_MalformedFrontmatter_SkippedWithWarning` / `Filter_MissingBin_Excluded` / `LoadForContext_ConcatenatesByName` / `DisabledSkill_NotLoaded` / `WorkspaceOverridesBuiltin` / `EnabledFalse_OptOut`.

`Parse_MalformedFrontmatter_SkippedWithWarning` is the most pedagogically loaded test: write a SKILL.md with broken YAML, confirm it's skipped rather than aborting the whole `List()`. That's nanobot's "one bad skill never breaks the pipeline" invariant, made executable in a Go test.

## Upstream Source Reading

s07 maps to `nanobot/agent/skills.py` — about 250 LOC. We carry over `SkillsLoader`, `_strip_frontmatter`, and `_check_requirements` directly to Go, then drop `metadata.nanobot/openclaw` (a compatibility layer for the OpenClaw project) and `build_skills_summary` (progressive loading — that's s08's territory).

Full annotated excerpt at [`upstream-readings/s07-skills-loader.py`](../../upstream-readings/s07-skills-loader.py).

```upstream:nanobot/agent/skills.py#L51-L73
def list_skills(self, filter_unavailable: bool = True) -> list[dict[str, str]]:
    skills = self._skill_entries_from_dir(self.workspace_skills, "workspace")
    workspace_names = {entry["name"] for entry in skills}
    if self.builtin_skills and self.builtin_skills.exists():
        skills.extend(
            self._skill_entries_from_dir(self.builtin_skills, "builtin", skip_names=workspace_names)
        )

    if self.disabled_skills:
        skills = [s for s in skills if s["name"] not in self.disabled_skills]

    if filter_unavailable:
        return [skill for skill in skills if self._check_requirements(self._get_skill_meta(skill["name"]))]
    return skills
```

```upstream:nanobot/agent/skills.py#L189-L196
def _check_requirements(self, skill_meta: dict) -> bool:
    """Check if skill requirements are met (bins, env vars)."""
    requires = skill_meta.get("requires", {})
    required_bins = requires.get("bins", [])
    required_env_vars = requires.get("env", [])
    return all(shutil.which(cmd) for cmd in required_bins) and all(
        os.environ.get(var) for var in required_env_vars
    )
```

**5 reading notes**:

- **Upstream's `_check_requirements` uses `shutil.which` + `os.environ.get`**; the Go version uses `exec.LookPath` + `os.Getenv`. Semantically identical, even the naming style maps cleanly across languages.
- **Upstream has a double-key `metadata.nanobot.requires`** — that's a compatibility layer because the same loader is shared with the OpenClaw project. The frontmatter contains a `metadata: {"nanobot": {"requires": ...}}` JSON-encoded string. We dropped the indirection: `requires` is at the top level of the frontmatter. Teaching versions should be clear above all.
- **Upstream's `_strip_frontmatter` uses a regex** (`re.compile(r"^---\s*\r?\n(.*?)\r?\n---\s*\r?\n?", re.DOTALL)`). The Go version hand-rolls a "find `---\n` opener, then find `\n---` closer" byte scan. One fewer dependency, more explicit logic — and it's easier to spot where CRLF normalization happens.
- **Upstream has `build_skills_summary`** — concatenates every skill's "name + description + path + availability" into a single Markdown block, telling the LLM "you have these skills available, you can `read_file` them on demand". This is *progressive loading*, an optimization s08's ContextBuilder will use. s07 doesn't ship it; that's the next chapter.
- **Upstream's `get_always_skills` honors `always: true`** — those skills auto-load every BUILD state without an explicit `LoadForContext` call. We don't ship that here for the same reason: it's a policy decision belonging to s08.

**Read further**:

1. `nanobot/agent/skills.py:215-242` — `get_skill_metadata` uses `yaml.safe_load` and silently returns `None` on `yaml.YAMLError`. That's the upstream embodiment of "one bad skill never breaks the loader".
2. `nanobot/skills/{github,weather,summarize,skill-creator}/SKILL.md` — the 4 builtin skills upstream ships. `skill-creator/SKILL.md` is particularly fun: it's a *skill that teaches the agent to write new skills* — one level of recursion.
3. `nanobot/agent/context.py:assemble_skills_section` — how s08's ContextBuilder drops the loader output into the system prompt. Reading this clarifies why `LoadForContext` returns `### Skill: <name>\n\n<body>\n\n---\n\n` separators: the format matches what ContextBuilder expects.

---

**Next**: s08 ContextBuilder uses both MemoryStore (s06) and SkillLoader (this chapter) to assemble the system prompt.
