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

// s09 wires SDKCaptureHook into the Runner. The flow is otherwise the
// same as s08: ContextBuilder builds the system prompt and messages,
// Runner.Run drives the inner loop. The only new wiring is:
//
//   captureHook := NewSDKCaptureHook()
//   runner.SetHook(captureHook)
//   result, _ := runner.Run(...)
//   fmt.Printf("[s09] tools used: %d %v\n",
//       len(captureHook.ToolsUsed), captureHook.ToolsUsed)
//
// The capture hook is the "default observability hook" upstream provides
// so SDK callers can read tools_used + final messages without writing
// their own hook. Composing it with custom hooks is a single line:
//
//   runner.SetHook(NewCompositeHook(captureHook, myCustomHook))

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
	maxHistory := flag.Int("max-history", 40,
		"max prior messages from the session to include in the prompt")
	maxTokens := flag.Int("max-system-tokens", 6000,
		"approximate token budget for prior history (uses len(s)/4)")
	verbose := flag.Bool("v", false, "verbose: print loaded skill + bootstrap counts on startup")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s09 [flags] <prompt>\n\n"+
				"  ANTHROPIC_API_KEY must be set.\n\n"+
				"  s09 wires SDKCaptureHook into the Runner. After Run returns,\n"+
				"  main prints len(captureHook.ToolsUsed) so you can see how the\n"+
				"  hook recorded tool invocations. The Runner now calls hooks at\n"+
				"  five well-defined moments: BeforeIter, OnStream (reserved),\n"+
				"  BeforeExecuteTools, AfterIter, FinalizeContent. Compose hooks\n"+
				"  via CompositeHook for telemetry + capture in one go.\n")
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
		fmt.Printf("[s09] bootstrap files present: %d/%d (%v)\n",
			bootstrapCount, len(BootstrapFiles), BootstrapFiles)
		fmt.Printf("[s09] loaded %d skill(s) from workspace=%q builtin=%q\n",
			len(loaded), skillsRoot, builtinRoot)
		for _, sk := range loaded {
			fmt.Printf("       - %s\n", sk.Name)
		}
		fmt.Printf("[s09] skills requested for system prompt: %v\n", requested)
	}

	provider := NewAnthropicProvider(apiKey, *model)
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(provider)

	// s09: wire SDKCaptureHook into the Runner. After Run returns the
	// hook holds .ToolsUsed and .FinalMessages. Compose with custom
	// hooks via NewCompositeHook(capture, mine) — the Runner takes a
	// single Hook so the composite is the canonical "many hooks at
	// once" pattern.
	captureHook := NewSDKCaptureHook()
	runner.SetHook(captureHook)

	sm := NewSessionManager()

	sess := sm.GetOrCreate(*sessionKey)
	rt := RuntimeMeta{
		Now:     time.Now(),
		Channel: "cli",
		Sender:  os.Getenv("USER"),
	}
	sysPrompt := ctxBuilder.BuildSystemPrompt(rt)
	msgs := ctxBuilder.BuildMessages(BuildSpec{
		Session:            sess,
		Runtime:            rt,
		UserText:           prompt,
		MaxHistoryMessages: *maxHistory,
		MaxSystemTokens:    *maxTokens,
	})

	if *verbose {
		fmt.Printf("[s09] system prompt: %d chars; messages: %d\n",
			len(sysPrompt), len(msgs))
	}

	spec := AgentRunSpec{
		InitialMessages: msgs,
		Tools:           registry,
		Model:           *model,
		MaxIterations:   *maxTurns,
		System:          sysPrompt,
		Verbose:         *verbose,
	}

	result, err := runner.Run(context.Background(), spec)
	if err != nil {
		log.Fatalf("runner: %v", err)
	}

	// SDKCaptureHook now holds the hook-recorded tool list and final
	// messages. Print a one-line summary so the operator can confirm
	// hook wiring worked end-to-end.
	tools := captureHook.ToolsUsedSnapshot()
	fmt.Printf("[s09] hook recorded %d tool invocation(s): %v\n", len(tools), tools)

	if _, err := mem.AppendHistory(HistoryEntry{Role: "user", Content: prompt}); err != nil {
		log.Printf("warn: persist user turn: %v", err)
	}
	if _, err := mem.AppendHistory(HistoryEntry{
		Role:      "assistant",
		Content:   result.FinalContent,
		ToolsUsed: result.ToolsUsed,
		Usage:     result.Usage,
	}); err != nil {
		log.Printf("warn: persist assistant turn: %v", err)
	}

	userMsg := Message{Role: "user", Content: []ContentBlock{{Type: "text", Text: prompt}}}
	sess.Append(userMsg)
	if len(result.Messages) > 0 {
		sess.AppendAll(result.Messages)
	}

	fmt.Println(result.FinalContent)
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
