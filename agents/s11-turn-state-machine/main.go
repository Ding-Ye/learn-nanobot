package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// s11 wires the new outer state machine on top of everything s01-s10
// taught. The Bus's per-session goroutine no longer calls Runner.Run
// directly — it calls Loop.Process, which walks the state graph
// (RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE)
// and produces a TurnContext with the final reply.
//
// In verbose mode (-v) you see each transition printed:
//
//	[s11] state: START -> RESTORE
//	[s11] state: RESTORE -> COMPACT
//	[s11] state: COMPACT -> COMMAND
//	[s11] state: COMMAND -> BUILD
//	[s11] state: BUILD -> RUN
//	[s11] state: RUN -> SAVE
//	[s11] state: SAVE -> RESPOND
//	[s11] state: RESPOND -> DONE
//
// Slash commands (/help, /clear) short-circuit straight to RESPOND;
// you'll see RESTORE → COMPACT → COMMAND → RESPOND → DONE.

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
	requestedSkills := flag.String("skills", "",
		"comma-separated list of skill names to inject into the system prompt")
	autocompactTTL := flag.Duration("autocompact-ttl", 5*time.Minute,
		"idle TTL for background session compaction; 0 disables AutoCompact")
	compactBudget := flag.Int("compact-budget", 0,
		"foreground compact threshold in tokens; 0 disables foreground compaction (only background AutoCompact runs)")
	verbose := flag.Bool("v", false, "verbose: print state transitions + bootstrap counts")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s11 [flags] <prompt>\n\n"+
				"  ANTHROPIC_API_KEY must be set.\n\n"+
				"  s11 introduces the outer state machine. Every turn walks\n"+
				"  RESTORE → COMPACT → COMMAND → BUILD → RUN → SAVE → RESPOND → DONE.\n"+
				"  Use -v to watch transitions; try '/help' to see the\n"+
				"  COMMAND state short-circuit straight to RESPOND.\n")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" && !strings.HasPrefix(strings.TrimSpace(prompt), "/") {
		// Slash commands short-circuit before any LLM call, so we
		// allow them through without an API key — useful for `/help`
		// and `/clear` smoke tests.
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

	var requested []string
	if *requestedSkills != "" {
		for _, name := range strings.Split(*requestedSkills, ",") {
			if name = strings.TrimSpace(name); name != "" {
				requested = append(requested, name)
			}
		}
	}

	ctxBuilder := NewContextBuilder(mem, skills, requested)

	if *verbose {
		loaded := skills.List()
		bootstrapCount := 0
		for _, name := range BootstrapFiles {
			if body, _ := mem.ReadMemory(name); strings.TrimSpace(body) != "" {
				bootstrapCount++
			}
		}
		fmt.Printf("[s11] bootstrap files present: %d/%d (%v)\n",
			bootstrapCount, len(BootstrapFiles), BootstrapFiles)
		fmt.Printf("[s11] loaded %d skill(s) from workspace=%q builtin=%q\n",
			len(loaded), skillsRoot, builtinRoot)
		for _, sk := range loaded {
			fmt.Printf("       - %s\n", sk.Name)
		}
		fmt.Printf("[s11] skills requested for system prompt: %v\n", requested)
		fmt.Printf("[s11] autocompact TTL: %s; compact budget: %d\n", *autocompactTTL, *compactBudget)
	}

	provider := NewAnthropicProvider(apiKey, *model)
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(provider)

	captureHook := NewSDKCaptureHook()
	runner.SetHook(captureHook)

	sm := NewSessionManager()
	consolidator := NewConsolidator(provider, mem)
	consolidator.Model = *model
	autocompact := NewAutoCompact(sm, consolidator, *autocompactTTL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if *autocompactTTL > 0 {
		go autocompact.Run(ctx, 30*time.Second, func() map[string]bool {
			return map[string]bool{*sessionKey: true}
		})
	}

	loop := NewLoop(LoopConfig{
		Runner:        runner,
		Tools:         registry,
		CtxBuilder:    ctxBuilder,
		Sessions:      sm,
		Memory:        mem,
		Consolidator:  consolidator,
		Hook:          captureHook,
		Model:         *model,
		MaxIter:       *maxTurns,
		CompactBudget: *compactBudget,
		Verbose:       *verbose,
	})

	tc, err := loop.Process(ctx, InboundMessage{
		SessionKey: *sessionKey,
		UserText:   prompt,
		ID:         fmt.Sprintf("cli-%d", time.Now().UnixNano()),
	})
	if err != nil {
		log.Fatalf("loop: %v (state=%s)", err, tc.State)
	}

	if *verbose {
		tools := captureHook.ToolsUsedSnapshot()
		fmt.Printf("[s11] hook recorded %d tool invocation(s): %v\n", len(tools), tools)
		if last := autocompact.LastSummary(*sessionKey); last != "" {
			fmt.Printf("[s11] autocompact summary: %.200s\n", last)
		}
		fmt.Printf("[s11] terminal state: %s\n", tc.State)
	}

	fmt.Println(tc.FinalContent)
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
func repoRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "testdata")); err == nil {
		return filepath.Dir(exe)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}
