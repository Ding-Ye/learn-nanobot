---
title: "s07 · 技能加载器（SkillLoader）"
chapter: 7
slug: s07-skills-loader
est_read_min: 22
---

# s07 · 技能加载器（SkillLoader）

> 教什么：给 agent 加技能（Skills）。一份 `SkillLoader` 走 `<workspace>/skills/` + builtin 两个目录、读 YAML frontmatter、用 `requires.bins` / `requires.env` 做实时可用性检查、最后把一组按名字选中的技能拼成一段 Markdown，准备给 s08 的 ContextBuilder 塞进 system prompt。**技能不是代码，是 Markdown**——LLM 自己读自己的 runbook。

---

## Problem / 问题

到 s06 为止，agent 拥有：provider 抽象、工具注册表、内部 LLM 循环、Session、MessageBus、文件系统记忆。但它的"行为"完全靠两件东西决定：

- **静态的 system prompt**（s01 起就只是字符串字面量），
- **runtime 的工具列表**（在 `Registry` 里 register 一遍）。

如果你要让一个 agent"会查天气"——不一定要写 Python 实现一个 `weather_api` 工具。在 nanobot 的世界里，更轻量的做法是：写一份 `SKILL.md`：

```markdown
---
name: weather
requires:
  bins: [curl]
---
要查天气，跑 `curl wttr.in/<城市>?format=3`。返回是人话。
```

把这份 Markdown 塞进 system prompt，LLM 看到就知道该 `bash` 工具调用 `curl wttr.in/...`——**根本不需要专门写一个 weather 工具**。技能就是 agent 自己的 runbook：用什么 CLI、什么 API、按什么节奏。

但这带来 4 个工程问题：

1. **从哪儿读？** 用户写的 skill 应该和 builtin 分开，但加载顺序得明确（用户的优先）。
2. **怎么解析？** YAML frontmatter + 自由 body，需要一个分割器。
3. **可用性怎么判？** `weather` skill 写"要装 `curl`"——如果 PATH 上没 curl，把它塞进 prompt 只会让 LLM 调一个失败的命令。需要 runtime guard。
4. **怎么禁用？** 操作员要能 `-disabled-skills weather`；作者也要能在 frontmatter 写 `enabled: false`。两条独立路径。

`SkillLoader` 就是把这四件事打包。

## Solution / 解决方案

新增 `skills.go`，引入 4 个类型 + 5 个函数：

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
    Enabled     *bool         `yaml:"enabled,omitempty"` // pointer = 三态
}

type Skill struct {
    Name              string
    Description       string
    Body              string  // 去掉 frontmatter 的 Markdown
    Path              string
    Available         bool
    UnavailableReason string
}

type SkillLoader struct { workspaceRoot, builtinRoot string; disabled map[string]bool }

func NewSkillLoader(workspace, builtin string, disabled []string) *SkillLoader
func (s *SkillLoader) List() []Skill
func (s *SkillLoader) Load(name string) (*Skill, bool)
func (s *SkillLoader) LoadForContext(names []string) string
func parseSkill(path string) (*parsedSkill, error)        // 内部
func checkRequirements(req SkillRequires) (bool, string)  // 内部
```

三层过滤，按顺序：

| 层 | 谁说了算 | 行为 |
|---|---|---|
| `disabled []string` | 操作员（CLI 标志） | 直接从 `List()` 里剔除 |
| `enabled: false`（frontmatter） | skill 作者 | 也从 `List()` 里剔除 |
| `requires.bins` / `requires.env` | runtime | 留在 `List()` 但 `Available=false`；`LoadForContext` 跳过 |

为什么三层不合并？因为它们的语义不同：
- 前两层是**配置**（"这台机器/这个仓库目前不希望这个 skill 出现"），
- 第三层是**可用性**（"这个 skill 存在，但现在跑不起来"）——s08 可能想在 prompt 里加一节"你有这些 skill 但目前不可用"，这要求第三层的 skill 仍出现在 `List()` 里。

`Enabled *bool` 是指针不是 bool：缺省（nil）= enabled，显式 `false` = opt-out。三态一个字段表完。

`workspace` 和 `builtin` 两个目录都走，**重名时 workspace 赢**——这是允许用户覆写 builtin skill 的关键（比如自己写一份 `weather` 用付费 API 替掉默认的 wttr.in）。

## How It Works / 工作原理

```ascii-anim frames=3
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│  main.go                                                         │
│    skills := NewSkillLoader(workspaceSkills, builtinSkills, ds)  │
│    if verbose { for sk := range skills.List() { print(sk) } }    │
│    // 还没把 skills 塞进 system prompt —— 那是 s08 的事         │
│                                                                  │
│  SkillLoader.List():                                             │
│    walk(workspaceRoot)  →  parseSkill(.../weather/SKILL.md)      │
│    walk(builtinRoot)    →  parseSkill(.../echo/SKILL.md)         │
│    去重(workspace 优先)                                         │
│    过滤(disabled / enabled:false)                                │
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
│  │   SKILL.md 进 system prompt 的样子：                        │  │
│  │                                                            │  │
│  │     ### Skill: weather                                     │  │
│  │     要查天气，跑 `curl wttr.in/<城市>?format=3`。            │  │
│  │     ---                                                    │  │
│  │     ### Skill: echo                                        │  │
│  │     用户要你回声，原样重复就行。                           │  │
│  │                                                            │  │
│  │   LLM 自己读自己的 runbook —— Go 不解析 body。              │  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

核心约 30 行（节选自 [`agents/s07-skills-loader/skills.go`](https://github.com/Ding-Ye/learn-nanobot/blob/main/agents/s07-skills-loader/skills.go)）：

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

**5 个非显然之处**：

1. **YAML frontmatter 是 metadata 与 content 之间唯一的 seam**。前面 4 行 `---/.../---` 是给 `SkillLoader` 看的；后面是给 LLM 看的 Markdown。**Go 代码不解析 body**——它只是字符串。LLM 拿到这段 Markdown 自己理解。
2. **`requires` 是 runtime guard，不是 declarative manifest**。Python 的 `pyproject.toml` 那种"声明式依赖"写一次就不动了；`requires.bins` 是每次 `List()` 都跑一次 `exec.LookPath`——你 `brew install curl` 之后不需要重启 agent，下一秒 weather skill 就 Available 了。
3. **`enabled` 是 `*bool` 不是 `bool`**。三态：`nil`（缺省，等价 enabled）、`false`（opt-out）、`true`（显式启用，no-op）。这个区分在 YAML 里很自然：作者写不写都行，写了也清楚。
4. **`Available=false` 的 skill 仍留在 `List()` 里**。它只是不会被 `LoadForContext` 渲染进 prompt。这给了 s08 一个选择：可以在 prompt 里加一段"你有这些 skill 但目前不可用，需要请告诉用户安装"，这要求第三层 skill 不被 `List()` 直接吃掉。
5. **一个坏 SKILL.md 不会拖垮整个 loader**。`parseSkill` 失败时，那个 skill 静默 skip，其他 skill 继续加载。这是 nanobot 的不变量："one bad skill never breaks the pipeline"——配上 race detector 和单元测试，这条不变量是有 enforcement 的。

## What Changed / 与上一节的变化

```diff
  // s06/main.go：只有 MemoryStore
- mem, _ := NewMemoryStore(root)
- ...
- bus.Send(...); out := <-bus.Out()
- mem.AppendHistory(...)

  // s07/main.go：多了 SkillLoader（但还没塞进 system prompt——那是 s08）
+ skillsRoot := filepath.Join(root, "skills")
+ builtinRoot := filepath.Join(repoRoot(), "testdata", "skills")
+ skills := NewSkillLoader(skillsRoot, builtinRoot, disabled)
+ if verbose { for _, sk := range skills.List() { fmt.Printf("  - %s\n", sk.Name) } }
  ...
  bus.Send(...)
```

新增类型（s07 的核心）：

```go
+ type SkillRequires struct { Bins, Env []string }
+ type SkillFrontmatter struct { Name, Description string; Requires SkillRequires; Enabled *bool }
+ type Skill struct { Name, Description, Body, Path string; Available bool; UnavailableReason string }
+ type SkillLoader struct { workspaceRoot, builtinRoot string; disabled map[string]bool }
```

新增依赖：

```go
require gopkg.in/yaml.v3 v3.0.1
```

`MemoryStore` / `SessionManager` / `Bus` / `Runner` / `Provider` / `Tool` / `Registry` 全部一行没改。这是 s07 的好处——它是**纯增量**，不动以前的任何骨架。

## Try It / 动手试一试

```bash
export ANTHROPIC_API_KEY=sk-ant-...
cd agents/s07-skills-loader

# 跑一次 verbose，看到加载了哪些 skill
go run . -workspace /tmp/agent-demo -v "list .go files"
# [s07] loaded 2 skill(s) from workspace=".../skills" builtin=".../testdata/skills"
#        - echo (ok) — Echo a string back as-is
#        - weather (ok) — Get current weather for a city
```

试 disabled 列表：

```bash
go run . -workspace /tmp/agent-demo -disabled-skills weather -v "test"
# [s07] loaded 1 skill(s)
#        - echo (ok) — Echo a string back as-is
```

试 workspace 覆写 builtin：

```bash
mkdir -p /tmp/agent-demo/skills/weather
cat > /tmp/agent-demo/skills/weather/SKILL.md <<'EOF'
---
name: weather
description: 我的付费天气接口
requires:
  bins: [curl]
---
用 https://my-weather.example.com，需要 X-API-Key 头。
EOF

go run . -workspace /tmp/agent-demo -v "test"
# - weather (ok) — 我的付费天气接口     ← workspace 版本赢了
```

试 missing-bin：

```bash
mkdir -p /tmp/agent-demo/skills/exotic
cat > /tmp/agent-demo/skills/exotic/SKILL.md <<'EOF'
---
name: exotic
description: 需要一个不存在的 CLI
requires:
  bins: [definitely-not-a-real-bin]
---
EOF

go run . -workspace /tmp/agent-demo -v "test"
# - exotic (unavailable: CLI: definitely-not-a-real-bin) — 需要一个不存在的 CLI
```

跑测试（race detector 必开）：

```bash
go test -count=1 -race -v ./...
```

期望 7 个测试全过：`Parse_Frontmatter_Standard` / `Parse_MalformedFrontmatter_SkippedWithWarning` / `Filter_MissingBin_Excluded` / `LoadForContext_ConcatenatesByName` / `DisabledSkill_NotLoaded` / `WorkspaceOverridesBuiltin` / `EnabledFalse_OptOut`。

`Parse_MalformedFrontmatter_SkippedWithWarning` 是这一节最有教学意义的测试：故意写一份 YAML 坏掉的 SKILL.md，确认它被 skip 而不是把整个 List() 都搞挂。这就是 nanobot 的不变量"一个坏 skill 不拖垮整个 loader"在 Go 测试里的具体体现。

## Upstream Source Reading / 上游源码阅读

s07 对应 `nanobot/agent/skills.py`——整文件 ~250 LOC。我们把上游的 `SkillsLoader`、`_strip_frontmatter`、`_check_requirements` 三块照搬到 Go 里，砍掉 `metadata.nanobot/openclaw`（那是兼容 OpenClaw 项目的层）和 `build_skills_summary`（那是给 progressive loading 用的，s08 才需要）。

完整注解版在 [`upstream-readings/s07-skills-loader.py`](../../upstream-readings/s07-skills-loader.py)。

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

**5 条对照阅读要点**：

- **上游 `_check_requirements` 用 `shutil.which` + `os.environ.get`**，我们 Go 版用 `exec.LookPath` + `os.Getenv`。语义完全等价，连命名风格都对得上。
- **上游有 `metadata.nanobot.requires` 双层 key**——这是上游同时兼容 nanobot 和 OpenClaw 两个项目的产物，文件最上面一行 frontmatter 里 `metadata: {"nanobot": {"requires": ...}}` 是嵌进去的 JSON 字符串。我们 Go 版砍掉了这层间接性，把 `requires` 提到 frontmatter 顶层——教学版要清晰。
- **上游 `_strip_frontmatter` 用 `re.compile(r"^---\s*\r?\n(.*?)\r?\n---\s*\r?\n?", re.DOTALL)`**，正则一把梭。我们 Go 版手写"找 `---\n` 开头、再找 `\n---` 闭合"的字节扫描，少一个 regex 包，逻辑更显式——也更容易看出 CRLF 处理在哪一步。
- **上游有 `build_skills_summary`**——把所有 skill 的"name + description + 路径 + 可用性"拼成一段 Markdown，让 LLM 知道存在哪些 skill 可以"按需读"（`read_file` 工具调取）。这叫 *progressive loading*，是 s08 ContextBuilder 的优化路径。我们 s07 不实现这个，留给 s08。
- **上游 `get_always_skills` 标记 `always: true` 的 skill**——这种 skill 不需要被显式 LoadForContext 调出来，每次 BUILD state 都自动进系统提示。我们 s07 也不实现，原因同上：那是 s08 的策略层。

**想读更多**：

1. `nanobot/agent/skills.py:215-242` —— `get_skill_metadata` 用 `yaml.safe_load`，遇到 `yaml.YAMLError` 静默返回 `None`。这就是"一个坏 skill 不拖垮 loader"的上游实现。
2. `nanobot/skills/{github,weather,summarize,skill-creator}/SKILL.md` —— 上游的 4 个 builtin skill。`skill-creator/SKILL.md` 特别有意思：它是一个**教 agent 写新 skill 的 skill**，递归一层。
3. `nanobot/agent/context.py:assemble_skills_section` —— s08 ContextBuilder 怎么把 SkillsLoader 的输出塞进 system prompt。看完这个再回头看 `LoadForContext` 的输出格式，你会发现 `### Skill: <name>\n\n<body>\n\n---\n\n` 这个分隔符是有讲究的。

---

**下一节预告**：s08 的 ContextBuilder 把 MemoryStore（s06）和 SkillLoader（this chapter）拼到一起，组装出最终的 system prompt。

Next: s08 ContextBuilder uses both MemoryStore (s06) and SkillLoader (this chapter) to assemble the system prompt.
