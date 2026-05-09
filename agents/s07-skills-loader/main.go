package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// providerProfiles — same menu as s01..s06.
var providerProfiles = map[string]struct {
	BaseURL string
	Model   string
	APIKey  string
}{
	"anthropic":  {Model: "claude-sonnet-4-6", APIKey: "ANTHROPIC_API_KEY"},
	"openai":     {BaseURL: "https://api.openai.com/v1", Model: "gpt-4o-mini", APIKey: "OPENAI_API_KEY"},
	"deepseek":   {BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat", APIKey: "DEEPSEEK_API_KEY"},
	"moonshot":   {BaseURL: "https://api.moonshot.cn/v1", Model: "moonshot-v1-8k", APIKey: "MOONSHOT_API_KEY"},
	"qwen":       {BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", Model: "qwen-plus", APIKey: "DASHSCOPE_API_KEY"},
	"groq":       {BaseURL: "https://api.groq.com/openai/v1", Model: "llama-3.3-70b-versatile", APIKey: "GROQ_API_KEY"},
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", Model: "openai/gpt-4o-mini", APIKey: "OPENROUTER_API_KEY"},
	"local":      {BaseURL: "http://localhost:8000/v1", Model: "local-model", APIKey: "OPENAI_API_KEY"},
}

func main() {
	maxTurns := flag.Int("max-turns", 20, "max agent iterations per turn before giving up")
	provider := flag.String("provider", envOr("PROVIDER", "anthropic"),
		"provider profile: anthropic | openai | deepseek | moonshot | qwen | groq | openrouter | local")
	baseURL := flag.String("base-url", envOr("BASE_URL", ""),
		"override the profile's base URL")
	modelFlag := flag.String("model", envOr("MODEL", ""),
		"override the profile's default model id")
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
			"usage: s07 [flags] <prompt>\n\n"+
				"  s07 wires Runner + Bus + MemoryStore (carry-over from s06) + a new\n"+
				"  SkillLoader. The SkillLoader walks <skills-dir> and <builtin-skills>,\n"+
				"  parses YAML frontmatter, filters by requires.bins / requires.env, and\n"+
				"  is ready for s08's ContextBuilder. -provider dispatches between\n"+
				"  Anthropic and OpenAI-compat endpoints.\n")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

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

	p, model, err := buildProvider(*provider, *baseURL, *modelFlag)
	if err != nil {
		log.Fatalf("%v", err)
	}
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(p)
	sm := NewSessionManager()
	bus := NewBus(sm, runner, registry, model, *maxTurns)

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

func buildProvider(name, baseURL, modelOverride string) (Provider, string, error) {
	prof, ok := providerProfiles[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown -provider %q (valid: %s)", name, validProviderNames())
	}
	apiKey := os.Getenv(prof.APIKey)
	if apiKey == "" {
		return nil, "", fmt.Errorf("%s is not set (required for -provider %s)", prof.APIKey, name)
	}
	model := modelOverride
	if model == "" {
		model = prof.Model
	}
	url := baseURL
	if url == "" {
		url = prof.BaseURL
	}
	if name == "anthropic" {
		return NewAnthropicProvider(apiKey, model), model, nil
	}
	return NewOpenAIProvider(apiKey, url, model), model, nil
}

func validProviderNames() string {
	names := make([]string, 0, len(providerProfiles))
	for k := range providerProfiles {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
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
