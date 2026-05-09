package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillLoader — the new module in s07. Mirrors upstream
// `nanobot/agent/skills.py::SkillsLoader`.
//
// A *skill* is a Markdown file (`SKILL.md`) with YAML frontmatter (name,
// description, requires) followed by a free-form body. The body is the
// guidance the agent reads at runtime — it might describe a runbook, a
// CLI invocation, or a workflow. **Skills are not code.** They're inserted
// verbatim into the system prompt; the LLM decides what to do with them.
//
// SkillLoader walks two directories:
//
//   <workspace>/skills/<name>/SKILL.md   — user-authored, takes precedence
//   <builtin>/<name>/SKILL.md            — bundled with the agent
//
// On a name collision the workspace skill wins — this is what allows users
// to override builtin skills (e.g. ship their own `weather` that uses a
// paid API instead of the free wttr.in default).
//
// Three layers of "is this skill usable?":
//
//   1. Disabled list — names in `disabled` are filtered out unconditionally.
//   2. Frontmatter `enabled: false` — skill author opted out.
//   3. requires.bins / requires.env — runtime guard. If `curl` isn't on
//      PATH, the weather skill is hidden so the LLM doesn't try to call
//      a tool that won't work.
//
// Layers 1 and 2 are *configuration*. Layer 3 is *availability*. We mark
// layer-3 skills as `Available: false` (with `UnavailableReason`) rather
// than dropping them, because s08's ContextBuilder may want to mention
// them in a "skills not available" section so the model knows they exist
// but can't be called right now.

// SkillRequires lists the runtime dependencies of a skill. Empty fields
// mean "no requirement of this kind".
type SkillRequires struct {
	Bins []string `yaml:"bins,omitempty"`
	Env  []string `yaml:"env,omitempty"`
}

// SkillFrontmatter is the YAML head of a SKILL.md file.
//
// The Enabled field is a *pointer* to a bool so we can distinguish three
// states: missing (nil → defaults to enabled), explicit `enabled: false`
// (opt-out), and explicit `enabled: true` (no-op but accepted). Mirrors
// upstream's behavior — absence implies enabled.
type SkillFrontmatter struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Requires    SkillRequires `yaml:"requires,omitempty"`
	Enabled     *bool         `yaml:"enabled,omitempty"`
}

// Skill is the materialized form: parsed frontmatter + body + on-disk
// path + an availability flag computed at load time.
type Skill struct {
	Name        string
	Description string
	Body        string // Markdown body with the frontmatter stripped
	Path        string // absolute path of the SKILL.md file

	// Available is false when requires.bins or requires.env aren't met.
	// UnavailableReason is a short human-readable summary (e.g.
	// "missing bin: curl" or "missing env: WEATHER_API_KEY") shown by
	// s08's ContextBuilder to inform the LLM why a skill is hidden.
	Available         bool
	UnavailableReason string
}

// SkillLoader is the front door. Construct one per agent (not per session
// — skills are agent-wide).
type SkillLoader struct {
	workspaceRoot string
	builtinRoot   string
	disabled      map[string]bool
}

// NewSkillLoader builds a SkillLoader. Either or both roots can be empty
// strings — an empty root is silently treated as "no skills here", which
// is what we want for tests that only want to exercise one source.
func NewSkillLoader(workspace, builtin string, disabled []string) *SkillLoader {
	d := make(map[string]bool, len(disabled))
	for _, name := range disabled {
		d[name] = true
	}
	return &SkillLoader{
		workspaceRoot: workspace,
		builtinRoot:   builtin,
		disabled:      d,
	}
}

// List walks both roots, parses every SKILL.md, dedupes by name (workspace
// overrides builtin), filters out disabled / `enabled: false` skills, and
// returns the result sorted by name. Skills with malformed frontmatter
// are *skipped*, not aborted — a single bad SKILL.md must not break the
// whole loader.
func (s *SkillLoader) List() []Skill {
	seen := map[string]bool{}
	var out []Skill

	// Workspace skills first so they win on name collision.
	for _, sk := range s.walk(s.workspaceRoot) {
		if s.disabled[sk.Name] {
			continue
		}
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		out = append(out, sk)
	}
	for _, sk := range s.walk(s.builtinRoot) {
		if s.disabled[sk.Name] {
			continue
		}
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		out = append(out, sk)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load returns the skill with the given name (or false if no such skill
// is registered / available). The lookup honors the same workspace > builtin
// precedence as List, and respects the disabled list.
func (s *SkillLoader) Load(name string) (*Skill, bool) {
	for _, sk := range s.List() {
		if sk.Name == name {
			cp := sk
			return &cp, true
		}
	}
	return nil, false
}

// LoadForContext renders the requested skills as Markdown sections,
// joined with "\n\n---\n\n" separators, suitable for inclusion in the
// system prompt. Names that don't resolve, or skills that are unavailable,
// are silently skipped. Mirrors upstream `load_skills_for_context`.
//
// We render even unavailable skills' descriptions if asked? — no. If a
// skill's `Available` is false it's not rendered, because the runtime
// guard says the LLM shouldn't call it. Use List() if you want to surface
// unavailable skills in a separate "not available" section.
func (s *SkillLoader) LoadForContext(names []string) string {
	skills := s.List()
	byName := make(map[string]Skill, len(skills))
	for _, sk := range skills {
		byName[sk.Name] = sk
	}

	var parts []string
	for _, name := range names {
		sk, ok := byName[name]
		if !ok || !sk.Available {
			continue
		}
		parts = append(parts, fmt.Sprintf("### Skill: %s\n\n%s", sk.Name, strings.TrimSpace(sk.Body)))
	}
	return strings.Join(parts, "\n\n---\n\n")
}

// walk lists every <root>/<name>/SKILL.md and returns them as parsed Skill
// structs, dropping any that fail to parse (logged via stderr in main but
// silently in unit tests so a single bad file doesn't fail the whole
// pipeline).
func (s *SkillLoader) walk(root string) []Skill {
	if root == "" {
		return nil
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		path := filepath.Join(root, name, "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			continue // no SKILL.md inside this dir
		}
		sk, err := parseSkill(path)
		if err != nil {
			// Malformed frontmatter or read error: skip but mark as
			// "exists but unusable". We construct a placeholder Skill so
			// `List()` *could* surface it (currently it filters by Available).
			// Tests assert skipping behavior; production callers can also
			// look at the file path that was skipped via List() if we
			// surfaced it — for now the contract is "skip silently".
			continue
		}
		// Frontmatter `enabled: false` is opt-out — skip entirely.
		if sk.frontmatterDisabled {
			continue
		}
		out = append(out, sk.skill)
	}
	return out
}

// parsedSkill is the internal carrier returned by parseSkill so we can
// distinguish "frontmatter said enabled:false" (skip silently) from
// "skill loaded fine, requirements check ran".
type parsedSkill struct {
	skill                Skill
	frontmatterDisabled  bool
}

// parseSkill reads a SKILL.md file, splits the YAML frontmatter from the
// Markdown body, runs the requirements check, and returns a Skill ready
// for List/LoadForContext.
//
// The frontmatter delimiter is `---\n` on its own line, both opening and
// closing. We accept CRLF too. If the file doesn't start with `---` we
// treat the whole file as body and use the directory name as the skill
// name, matching upstream's lenient behavior for partially-authored
// skills.
func parseSkill(path string) (*parsedSkill, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parseSkill read %s: %w", path, err)
	}
	body := string(raw)
	body = strings.ReplaceAll(body, "\r\n", "\n")

	fm := SkillFrontmatter{}
	rest := body

	if strings.HasPrefix(body, "---\n") {
		// Find the closing --- on its own line.
		closeIdx := strings.Index(body[4:], "\n---")
		if closeIdx < 0 {
			return nil, errors.New("parseSkill: opening --- has no matching closing ---")
		}
		yamlText := body[4 : 4+closeIdx]
		// rest starts after the closing --- and its trailing newline.
		afterClose := 4 + closeIdx + len("\n---")
		// Skip a single trailing \n if present (the closing line ends with \n).
		if afterClose < len(body) && body[afterClose] == '\n' {
			afterClose++
		}
		rest = body[afterClose:]
		if err := yaml.Unmarshal([]byte(yamlText), &fm); err != nil {
			return nil, fmt.Errorf("parseSkill yaml: %w", err)
		}
	}

	// Skill name: prefer frontmatter, else directory name.
	name := fm.Name
	if name == "" {
		name = filepath.Base(filepath.Dir(path))
	}

	// Frontmatter `enabled: false` short-circuits.
	if fm.Enabled != nil && !*fm.Enabled {
		return &parsedSkill{
			skill:                Skill{Name: name, Path: path, Body: rest, Description: fm.Description},
			frontmatterDisabled:  true,
		}, nil
	}

	available, reason := checkRequirements(fm.Requires)

	return &parsedSkill{
		skill: Skill{
			Name:              name,
			Description:       fm.Description,
			Body:              strings.TrimLeft(rest, "\n"),
			Path:              path,
			Available:         available,
			UnavailableReason: reason,
		},
	}, nil
}

// checkRequirements runs `exec.LookPath` for each required binary and
// `os.Getenv` for each required env var. Returns (true, "") if everything
// is satisfied, otherwise (false, reason) with a comma-joined list of
// what's missing — the same format upstream uses in
// `_get_missing_requirements`.
func checkRequirements(req SkillRequires) (bool, string) {
	var missing []string
	for _, bin := range req.Bins {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, "CLI: "+bin)
		}
	}
	for _, env := range req.Env {
		if os.Getenv(env) == "" {
			missing = append(missing, "ENV: "+env)
		}
	}
	if len(missing) == 0 {
		return true, ""
	}
	return false, strings.Join(missing, ", ")
}

