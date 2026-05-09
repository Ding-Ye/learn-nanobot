package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill writes a SKILL.md under <root>/<name>/SKILL.md and creates the
// directory if needed. Returns the absolute path of the created file.
func writeSkill(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestSkillLoader_Parse_Frontmatter_Standard — happy path: a well-formed
// SKILL.md with `name`, `description`, and `requires.bins` parses fully.
// All fields appear in the resulting Skill struct, the Markdown body is
// preserved verbatim (minus the frontmatter), and Available reflects whether
// the bin exists on PATH.
func TestSkillLoader_Parse_Frontmatter_Standard(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "weather", `---
name: weather
description: Get current weather for a city
requires:
  bins: [bash]
---
The body of the skill goes here.
`)

	loader := NewSkillLoader(root, "", nil)
	skills := loader.List()
	if len(skills) != 1 {
		t.Fatalf("List() len = %d, want 1 (skills=%+v)", len(skills), skills)
	}
	sk := skills[0]
	if sk.Name != "weather" {
		t.Errorf("Name = %q, want weather", sk.Name)
	}
	if sk.Description != "Get current weather for a city" {
		t.Errorf("Description = %q", sk.Description)
	}
	if !sk.Available {
		t.Errorf("Available = false (reason=%q); bash should always exist on PATH", sk.UnavailableReason)
	}
	if !strings.Contains(sk.Body, "The body of the skill goes here.") {
		t.Errorf("Body missing expected text: %q", sk.Body)
	}
	// Body should NOT include the leading frontmatter markers.
	if strings.HasPrefix(sk.Body, "---") {
		t.Errorf("Body still includes frontmatter: %q", sk.Body)
	}
}

// TestSkillLoader_Parse_MalformedFrontmatter_SkippedWithWarning — a SKILL.md
// with corrupt YAML frontmatter must be silently skipped. The loader's
// invariant is "one bad skill never breaks the whole pipeline" — other
// skills in the same directory must still load. The malformed skill does
// not appear in List().
func TestSkillLoader_Parse_MalformedFrontmatter_SkippedWithWarning(t *testing.T) {
	root := t.TempDir()
	// Bad YAML: unclosed bracket, mismatched indentation.
	writeSkill(t, root, "broken", `---
name: broken
description: this skill is corrupt
requires:
  bins: [unterminated
---
body
`)
	// A working sibling skill must still load.
	writeSkill(t, root, "working", `---
name: working
description: this one is fine
---
body
`)

	loader := NewSkillLoader(root, "", nil)
	skills := loader.List()

	if len(skills) != 1 {
		t.Fatalf("List() len = %d, want 1 (only the working skill); got %+v", len(skills), skills)
	}
	if skills[0].Name != "working" {
		t.Errorf("the surviving skill should be 'working', got %q", skills[0].Name)
	}
}

// TestSkillLoader_Filter_MissingBin_Excluded — a skill that requires a
// binary that doesn't exist must end up with Available=false and an
// UnavailableReason that names the missing bin. LoadForContext skips it.
func TestSkillLoader_Filter_MissingBin_Excluded(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "weather", `---
name: weather
description: pretend weather skill
requires:
  bins: [definitely-not-a-real-bin-x42]
---
You shouldn't see this body in LoadForContext output.
`)

	loader := NewSkillLoader(root, "", nil)
	sk, ok := loader.Load("weather")
	if !ok {
		t.Fatalf("Load(weather) = !ok; the skill should still be visible in List even when unavailable")
	}
	if sk.Available {
		t.Errorf("Available = true; expected false because the required bin is bogus")
	}
	if !strings.Contains(sk.UnavailableReason, "definitely-not-a-real-bin-x42") {
		t.Errorf("UnavailableReason should name the missing bin; got %q", sk.UnavailableReason)
	}

	// LoadForContext must skip the unavailable skill.
	rendered := loader.LoadForContext([]string{"weather"})
	if rendered != "" {
		t.Errorf("LoadForContext rendered an unavailable skill: %q", rendered)
	}
}

// TestSkillLoader_LoadForContext_ConcatenatesByName — register two skills,
// request both, and verify the rendered Markdown contains both bodies in
// the requested order, separated by `---`.
func TestSkillLoader_LoadForContext_ConcatenatesByName(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "echo", `---
name: echo
description: echo a string back
---
Repeat the user's input verbatim.
`)
	writeSkill(t, root, "shout", `---
name: shout
description: uppercase the input
---
Convert input to ALL CAPS.
`)

	loader := NewSkillLoader(root, "", nil)
	rendered := loader.LoadForContext([]string{"echo", "shout"})

	if !strings.Contains(rendered, "### Skill: echo") {
		t.Errorf("missing echo header: %q", rendered)
	}
	if !strings.Contains(rendered, "### Skill: shout") {
		t.Errorf("missing shout header: %q", rendered)
	}
	if !strings.Contains(rendered, "Repeat the user's input verbatim.") {
		t.Errorf("missing echo body: %q", rendered)
	}
	if !strings.Contains(rendered, "Convert input to ALL CAPS.") {
		t.Errorf("missing shout body: %q", rendered)
	}
	// The separator between the two sections.
	if !strings.Contains(rendered, "\n\n---\n\n") {
		t.Errorf("missing section separator: %q", rendered)
	}
	// Ordering: echo header should appear before shout header.
	echoIdx := strings.Index(rendered, "### Skill: echo")
	shoutIdx := strings.Index(rendered, "### Skill: shout")
	if echoIdx > shoutIdx {
		t.Errorf("section order wrong: echo at %d, shout at %d", echoIdx, shoutIdx)
	}
}

// TestSkillLoader_DisabledSkill_NotLoaded — when a skill name is in the
// disabled list passed to NewSkillLoader, it must not appear in List or
// resolve via Load — even though it exists on disk and is otherwise
// available.
func TestSkillLoader_DisabledSkill_NotLoaded(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "weather", `---
name: weather
description: weather skill
---
body
`)
	writeSkill(t, root, "echo", `---
name: echo
description: echo skill
---
body
`)

	loader := NewSkillLoader(root, "", []string{"weather"})

	skills := loader.List()
	if len(skills) != 1 {
		t.Fatalf("List() len = %d, want 1 (echo only); got %+v", len(skills), skills)
	}
	if skills[0].Name != "weather" && skills[0].Name == "weather" {
		t.Errorf("disabled weather still appeared")
	}
	if skills[0].Name != "echo" {
		t.Errorf("expected echo, got %q", skills[0].Name)
	}

	if _, ok := loader.Load("weather"); ok {
		t.Errorf("Load(weather) returned ok; should be filtered by disabled list")
	}
	if _, ok := loader.Load("echo"); !ok {
		t.Errorf("Load(echo) returned !ok; should still be reachable")
	}
}

// TestSkillLoader_WorkspaceOverridesBuiltin — when the same name exists
// in both workspace and builtin roots, the workspace version wins. This
// is what enables users to customize a builtin skill (e.g. ship their own
// `weather` skill that uses a paid API instead of the free wttr.in default).
//
// Bonus test (beyond the 5-required minimum) because the precedence rule
// is the whole point of having two roots — worth pinning down.
func TestSkillLoader_WorkspaceOverridesBuiltin(t *testing.T) {
	workspace := t.TempDir()
	builtin := t.TempDir()

	writeSkill(t, builtin, "weather", `---
name: weather
description: builtin version
---
This is the BUILTIN body.
`)
	writeSkill(t, workspace, "weather", `---
name: weather
description: workspace override
---
This is the WORKSPACE body.
`)

	loader := NewSkillLoader(workspace, builtin, nil)
	sk, ok := loader.Load("weather")
	if !ok {
		t.Fatalf("Load(weather) = !ok")
	}
	if sk.Description != "workspace override" {
		t.Errorf("description = %q, want workspace override", sk.Description)
	}
	if !strings.Contains(sk.Body, "WORKSPACE body") {
		t.Errorf("body should be from workspace, got %q", sk.Body)
	}
	if strings.Contains(sk.Body, "BUILTIN body") {
		t.Errorf("body should not include builtin: %q", sk.Body)
	}
}

// TestSkillLoader_EnabledFalse_OptOut — frontmatter `enabled: false` is the
// author's opt-out: even if requirements are met and the skill isn't on
// the disabled list, it must not appear in List.
func TestSkillLoader_EnabledFalse_OptOut(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "draft", `---
name: draft
description: a skill being authored
enabled: false
---
This skill is hidden until enabled is removed.
`)
	writeSkill(t, root, "ready", `---
name: ready
description: a normal skill
---
body
`)

	loader := NewSkillLoader(root, "", nil)
	skills := loader.List()
	if len(skills) != 1 {
		t.Fatalf("List() len = %d, want 1 (ready only); got %+v", len(skills), skills)
	}
	if skills[0].Name != "ready" {
		t.Errorf("expected ready, got %q", skills[0].Name)
	}
}
