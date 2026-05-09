package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	maxTurns := flag.Int("max-turns", 20, "max agent iterations per turn before giving up")
	model := flag.String("model", envOr("MODEL", "claude-sonnet-4-6"),
		"Anthropic model id (default claude-sonnet-4-6)")
	sessionKey := flag.String("session", "cli:direct",
		"session key to send the message under (one CLI run = one session by default)")
	workspace := flag.String("workspace", "",
		"workspace root directory (default ~/.learn-nanobot/agents/default)")
	skillsDir := flag.String("skills-dir", "",
		"workspace skills directory (default <workspace>/skills)")
	builtinSkills := flag.String("builtin-skills", "",
		"builtin skills directory; default uses the in-repo testdata/skills/")
	disabledSkills := flag.String("disabled-skills", "",
		"comma-separated list of skill names to disable")
	verbose := flag.Bool("v", false, "verbose: print loaded skill summary on startup")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s07 [-max-turns N] [-model ID] [-session KEY] [-workspace DIR] [-skills-dir DIR] [-disabled-skills NAMES] [-v] <prompt>\n\n"+
				"  ANTHROPIC_API_KEY must be set.\n\n"+
				"  s07 wires Runner + Bus + MemoryStore (carry-over from s06) + a new\n"+
				"  SkillLoader. The SkillLoader walks <skills-dir> and <builtin-skills>,\n"+
				"  parses YAML frontmatter, filters by requires.bins / requires.env, and\n"+
				"  is ready for s08's ContextBuilder. s07 itself doesn't yet inject the\n"+
				"  loaded skills into the system prompt — it just prints the count when\n"+
				"  -v is set, so you can confirm the loader works.\n")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		log.Fatalf("ANTHROPIC_API_KEY is not set")
	}

	root, err := resolveWorkspace(*workspace)
	if err != nil {
		log.Fatalf("workspace: %v", err)
	}
	mem, err := NewMemoryStore(root)
	if err != nil {
		log.Fatalf("memory store: %v", err)
	}

	skillsRoot := *skillsDir
	if skillsRoot == "" {
		skillsRoot = filepath.Join(root, "skills")
	}
	builtinRoot := *builtinSkills
	if builtinRoot == "" {
		// Default to the in-repo testdata so the demo "just works".
		// In a real deployment this would point to the bundled skills/ dir.
		builtinRoot = filepath.Join(repoRoot(), "testdata", "skills")
	}
	var disabled []string
	if *disabledSkills != "" {
		for _, name := range strings.Split(*disabledSkills, ",") {
			if name = strings.TrimSpace(name); name != "" {
				disabled = append(disabled, name)
			}
		}
	}
	skills := NewSkillLoader(skillsRoot, builtinRoot, disabled)

	if *verbose {
		loaded := skills.List()
		fmt.Printf("[s07] loaded %d skill(s) from workspace=%q builtin=%q\n",
			len(loaded), skillsRoot, builtinRoot)
		for _, sk := range loaded {
			status := "ok"
			if !sk.Available {
				status = "unavailable: " + sk.UnavailableReason
			}
			fmt.Printf("       - %s (%s) — %s\n", sk.Name, status, sk.Description)
		}
	}

	provider := NewAnthropicProvider(apiKey, *model)
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(provider)
	sm := NewSessionManager()
	bus := NewBus(sm, runner, registry, *model, *maxTurns)

	bus.Send(InboundMessage{
		SessionKey: *sessionKey,
		UserText:   prompt,
		ID:         "cli-1",
	})

	out := <-bus.Out()
	if out.Err != nil {
		bus.Stop()
		log.Fatalf("bus error: %v", out.Err)
	}

	// Persist this turn (carry-over from s06).
	if _, err := mem.AppendHistory(HistoryEntry{Role: "user", Content: prompt}); err != nil {
		log.Printf("warn: persist user turn: %v", err)
	}
	if _, err := mem.AppendHistory(HistoryEntry{
		Role:      "assistant",
		Content:   out.Text,
		ToolsUsed: out.Result.ToolsUsed,
		Usage:     out.Result.Usage,
	}); err != nil {
		log.Printf("warn: persist assistant turn: %v", err)
	}

	fmt.Println(out.Text)

	bus.Stop()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveWorkspace returns the on-disk root for MemoryStore.
func resolveWorkspace(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no $HOME and no -workspace given: %w", err)
	}
	return filepath.Join(home, ".learn-nanobot", "agents", "default"), nil
}

// repoRoot returns the directory containing this main.go binary's source.
// Used to default `-builtin-skills` to the in-repo testdata for demos.
// This is best-effort — if the binary was built elsewhere we just return ".".
func repoRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	// `go run .` puts the binary in a tmp dir, so resolving via os.Executable
	// is unreliable for development. Fall back to current working directory
	// which during `go run .` is the source directory.
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "testdata")); err == nil {
		return filepath.Dir(exe)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}
