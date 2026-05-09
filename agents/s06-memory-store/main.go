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

// providerProfiles — same menu as s01..s05.
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
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s06 [flags] <prompt>\n\n"+
				"  s06 wires Runner + Bus + a real on-disk MemoryStore. Each turn is\n"+
				"  appended to <workspace>/history.jsonl and the cursor lands in\n"+
				"  <workspace>/.cursor. MEMORY.md / SOUL.md / USER.md are read on demand;\n"+
				"  s10 (Consolidator) and App. A (Dream) are the writers. -provider\n"+
				"  dispatches between Anthropic and OpenAI-compat endpoints.\n")
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

	// Persist this turn. We record the user prompt and the assistant's
	// final reply as two HistoryEntry rows. Note: AppendHistory advances
	// the cursor on disk; if the agent crashes before this returns, the
	// cursor and the line count will already be in sync because of the
	// atomic-rename on `.cursor`.
	if _, err := mem.AppendHistory(HistoryEntry{Role: "user", Content: prompt}); err != nil {
		log.Printf("warn: persist user turn: %v", err)
	}
	cursor, err := mem.AppendHistory(HistoryEntry{
		Role:      "assistant",
		Content:   out.Text,
		ToolsUsed: out.Result.ToolsUsed,
		Usage:     out.Result.Usage,
	})
	if err != nil {
		log.Printf("warn: persist assistant turn: %v", err)
	} else {
		// LastConsolidated is the cursor of the most recent line that
		// the Consolidator (s10) has summarized. Until s10 lands we leave
		// it at zero — but we keep `cursor` referenced here so the
		// AppendHistory call's return value isn't quietly dropped, and so
		// future s10 wiring is one line away.
		_ = cursor
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func validProviderNames() string {
	names := make([]string, 0, len(providerProfiles))
	for k := range providerProfiles {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// resolveWorkspace returns the on-disk root for MemoryStore. If -workspace
// is non-empty, it's used as-is (parents created by NewMemoryStore). If
// empty, defaults to ~/.learn-nanobot/agents/default.
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
