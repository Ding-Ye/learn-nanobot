# Source: HKUDS/nanobot · nanobot/agent/skills.py
# License: MIT
# Fetched: 2026-05-09 from main branch (~242 LOC)
#
# Teaching excerpt for learn-nanobot/s07-skills-loader. We carry over the
# three load-bearing pieces of `SkillsLoader`:
#
#   1. list_skills        — walk workspace + builtin, dedupe, filter
#   2. _check_requirements — runtime guard (bins on PATH, env set)
#   3. _strip_frontmatter  — split YAML head from Markdown body
#
# Skipped:
#
#   - build_skills_summary / get_always_skills — progressive-loading
#     policy that belongs to s08's ContextBuilder, not s07's loader.
#   - _parse_nanobot_metadata — OpenClaw-compat indirection that we
#     deliberately drop in the Go translation (we put `requires` at the
#     top of the frontmatter directly).

# ────────────────────────────────────────────────────────────────────
# Part 1. Constructor — the two-root + disabled-set design
# ────────────────────────────────────────────────────────────────────

import json
import os
import re
import shutil
from pathlib import Path

import yaml

# Default builtin skills directory (relative to this file).
# learn-nanobot/s07 ships its testdata/skills/ as the demo equivalent;
# in production you'd point this at a packaged-with-the-binary directory.
BUILTIN_SKILLS_DIR = Path(__file__).parent.parent / "skills"

# The frontmatter regex. Anchored at start of file. Captures everything
# between the opening `---\n` and the closing `\n---\s*\r?\n?`.
# learn-nanobot's Go port hand-rolls this scan rather than using regex,
# but the logic is the same.
_STRIP_SKILL_FRONTMATTER = re.compile(
    r"^---\s*\r?\n(.*?)\r?\n---\s*\r?\n?",
    re.DOTALL,
)


class SkillsLoader:
    """
    Loader for agent skills.

    Skills are markdown files (SKILL.md) that teach the agent how to use
    specific tools or perform certain tasks.
    """

    def __init__(
        self,
        workspace: Path,
        builtin_skills_dir: Path | None = None,
        disabled_skills: set[str] | None = None,
    ):
        # Two roots and a disabled set — exactly the same shape as
        # learn-nanobot/s07's SkillLoader struct: workspaceRoot,
        # builtinRoot, disabled.
        self.workspace = workspace
        self.workspace_skills = workspace / "skills"
        self.builtin_skills = builtin_skills_dir or BUILTIN_SKILLS_DIR
        self.disabled_skills = disabled_skills or set()


# ────────────────────────────────────────────────────────────────────
# Part 2. list_skills — workspace > builtin precedence + disabled filter
# ────────────────────────────────────────────────────────────────────

    def list_skills(self, filter_unavailable: bool = True) -> list[dict[str, str]]:
        """
        List all available skills.

        Args:
            filter_unavailable: If True, filter out skills with unmet requirements.

        Returns:
            List of skill info dicts with 'name', 'path', 'source'.
        """
        # Walk workspace first; remember those names so we can skip
        # builtins with the same name. THIS is the precedence rule
        # learn-nanobot/s07 mirrors with `seen[sk.Name] = true` in walk().
        skills = self._skill_entries_from_dir(self.workspace_skills, "workspace")
        workspace_names = {entry["name"] for entry in skills}
        if self.builtin_skills and self.builtin_skills.exists():
            skills.extend(
                self._skill_entries_from_dir(
                    self.builtin_skills, "builtin", skip_names=workspace_names
                )
            )

        # Operator-level disable filter. Pure config concern.
        if self.disabled_skills:
            skills = [s for s in skills if s["name"] not in self.disabled_skills]

        # Runtime availability filter. NOTE: filter_unavailable=True is
        # the default — but the upstream API leaves a knob. learn-nanobot's
        # Go port flips this to "always include unavailable in List(),
        # always exclude in LoadForContext" — which gives s08 freedom to
        # surface unavailable skills in a separate prompt section.
        if filter_unavailable:
            return [
                skill for skill in skills
                if self._check_requirements(self._get_skill_meta(skill["name"]))
            ]
        return skills


# ────────────────────────────────────────────────────────────────────
# Part 3. _check_requirements — the runtime guard
# ────────────────────────────────────────────────────────────────────

    def _check_requirements(self, skill_meta: dict) -> bool:
        """Check if skill requirements are met (bins, env vars)."""
        # `shutil.which` returns the resolved path of the binary on PATH
        # (or None). learn-nanobot/s07 uses `exec.LookPath` which returns
        # an err for the missing case — semantically identical.
        requires = skill_meta.get("requires", {})
        required_bins = requires.get("bins", [])
        required_env_vars = requires.get("env", [])
        return all(shutil.which(cmd) for cmd in required_bins) and all(
            os.environ.get(var) for var in required_env_vars
        )

    def _get_missing_requirements(self, skill_meta: dict) -> str:
        """Get a description of missing requirements."""
        # Used by build_skills_summary (which we omit) but also useful to
        # mimic in learn-nanobot/s07's UnavailableReason. The "CLI: foo,
        # ENV: BAR" format is what we picked.
        requires = skill_meta.get("requires", {})
        required_bins = requires.get("bins", [])
        required_env_vars = requires.get("env", [])
        return ", ".join(
            [f"CLI: {cmd}" for cmd in required_bins if not shutil.which(cmd)]
            + [f"ENV: {var}" for var in required_env_vars if not os.environ.get(var)]
        )


# ────────────────────────────────────────────────────────────────────
# Part 4. _strip_frontmatter + load_skills_for_context — the rendering
# ────────────────────────────────────────────────────────────────────

    def _strip_frontmatter(self, content: str) -> str:
        """Remove YAML frontmatter from markdown content."""
        # Both upstream and the Go port treat "no opening ---" as
        # "no frontmatter, use the whole thing as body, fall back to
        # directory name for the skill name".
        if not content.startswith("---"):
            return content
        match = _STRIP_SKILL_FRONTMATTER.match(content)
        if match:
            return content[match.end():].strip()
        return content

    def load_skill(self, name: str) -> str | None:
        """
        Load a skill by name.

        Args:
            name: Skill name (directory name).

        Returns:
            Skill content or None if not found.
        """
        # Same workspace > builtin order as list_skills.
        roots = [self.workspace_skills]
        if self.builtin_skills:
            roots.append(self.builtin_skills)
        for root in roots:
            path = root / name / "SKILL.md"
            if path.exists():
                return path.read_text(encoding="utf-8")
        return None

    def load_skills_for_context(self, skill_names: list[str]) -> str:
        """
        Load specific skills for inclusion in agent context.

        Args:
            skill_names: List of skill names to load.

        Returns:
            Formatted skills content.
        """
        # The output format learn-nanobot/s07 mirrors:
        #
        #   ### Skill: <name>
        #
        #   <body>
        #
        #   ---
        #
        #   ### Skill: <next>
        #
        # The "\n\n---\n\n" separator between sections is significant —
        # s08's ContextBuilder relies on it for layout.
        parts = [
            f"### Skill: {name}\n\n{self._strip_frontmatter(markdown)}"
            for name in skill_names
            if (markdown := self.load_skill(name))
        ]
        return "\n\n---\n\n".join(parts)

    def get_skill_metadata(self, name: str) -> dict | None:
        """
        Get metadata from a skill's frontmatter.

        Args:
            name: Skill name.

        Returns:
            Metadata dict or None.
        """
        # The "one bad SKILL.md never breaks the loader" invariant lives
        # here: any yaml.YAMLError is caught and returned as None. The
        # caller (list_skills via _check_requirements) treats None as
        # "unknown requires, skip filtering" — which means the skill
        # is unaffected if its YAML is broken. learn-nanobot/s07's Go port
        # is stricter: malformed YAML → skill skipped entirely. Pick your
        # tradeoff: lenient-include vs. strict-skip. The Go side is more
        # defensive because the SkillLoader has no logger to warn through.
        content = self.load_skill(name)
        if not content or not content.startswith("---"):
            return None
        match = _STRIP_SKILL_FRONTMATTER.match(content)
        if not match:
            return None
        try:
            parsed = yaml.safe_load(match.group(1))
        except yaml.YAMLError:
            return None  # ← the invariant in action
        if not isinstance(parsed, dict):
            return None
        metadata: dict[str, object] = {}
        for key, value in parsed.items():
            metadata[str(key)] = value
        return metadata


# ────────────────────────────────────────────────────────────────────
# Part 5. The OpenClaw indirection we deliberately drop
# ────────────────────────────────────────────────────────────────────

    def _parse_nanobot_metadata(self, raw: object) -> dict:
        """Extract nanobot/openclaw metadata from a frontmatter field.

        ``raw`` may be a dict (already parsed by yaml.safe_load) or a JSON str.
        """
        # This is the bit learn-nanobot/s07 deliberately removes. Upstream
        # supports two-key frontmatter so the same SKILL.md can be loaded
        # by both `nanobot` and `openclaw`. The frontmatter contains:
        #
        #   metadata: {"nanobot": {"requires": {"bins": ["curl"]}}}
        #
        # ...which is a JSON-encoded string inside a YAML field. The Go
        # port flattens this to:
        #
        #   requires:
        #     bins: [curl]
        #
        # Same expressive power, far less indirection. If you want the
        # OpenClaw shape later, port _parse_nanobot_metadata back.
        if isinstance(raw, dict):
            data = raw
        elif isinstance(raw, str):
            try:
                data = json.loads(raw)
            except (json.JSONDecodeError, TypeError):
                return {}
        else:
            return {}
        if not isinstance(data, dict):
            return {}
        payload = data.get("nanobot", data.get("openclaw", {}))
        return payload if isinstance(payload, dict) else {}

    def _get_skill_meta(self, name: str) -> dict:
        """Get nanobot metadata for a skill (cached in frontmatter)."""
        raw_meta = self.get_skill_metadata(name) or {}
        return self._parse_nanobot_metadata(raw_meta.get("metadata"))


# ────────────────────────────────────────────────────────────────────
# Read further (not in this excerpt):
# ────────────────────────────────────────────────────────────────────
#
# - build_skills_summary (lines 111-142)  → s08 progressive loading
# - get_always_skills    (lines 203-213)  → s08 always-on policy
# - _skill_entries_from_dir (lines 35-49) → file walking helper, ported
#   in Go as SkillLoader.walk()
