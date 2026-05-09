package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
)

// providerProfiles — same menu as s01..s04. Anthropic uses its native
// /v1/messages; everything else goes through the OpenAI-compat translator.
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
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s05 [flags] <prompt>\n\n"+
				"  s05 wires Runner behind a SessionManager + Bus. The CLI sends one\n"+
				"  inbound message under -session (default cli:direct) and waits for one\n"+
				"  outbound. Run multiple times with the same -session to keep history.\n"+
				"  -provider dispatches between Anthropic and OpenAI-compat endpoints.\n")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

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

	// Wait for one reply. In a real driver (s11) you'd run this loop
	// forever and dispatch outbound messages back to whatever channel sent
	// the inbound. Here we just print the first reply and stop.
	out := <-bus.Out()
	if out.Err != nil {
		bus.Stop()
		log.Fatalf("bus error: %v", out.Err)
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
