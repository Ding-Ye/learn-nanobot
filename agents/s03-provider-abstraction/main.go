package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
)

// providerProfiles lets s03 dispatch between AnthropicProvider and
// OpenAIProvider via -provider. The interface is the s03+ Provider
// (Chat → LLMResponse), so both impls plug into the same Loop without
// changes upstream.
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
	verbose := flag.Bool("v", false, "print every turn (assistant text + tool calls)")
	maxTurns := flag.Int("max-turns", 20, "max agent turns before giving up")
	provider := flag.String("provider", envOr("PROVIDER", "anthropic"),
		"provider profile: anthropic | openai | deepseek | moonshot | qwen | groq | openrouter | local")
	baseURL := flag.String("base-url", envOr("BASE_URL", ""),
		"override the profile's base URL")
	modelFlag := flag.String("model", envOr("MODEL", ""),
		"override the profile's default model id")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s03 [-v] [-max-turns N] [-provider P] [-base-url URL] [-model ID] <prompt>\n\n"+
				"  s03 reshapes Provider into a normalized LLMResponse-returning\n"+
				"  interface. Both AnthropicProvider and OpenAIProvider implement\n"+
				"  the same Chat method; -provider dispatches at startup.\n")
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

	loop := &Loop{
		Provider: p,
		Tools:    registry,
		MaxTurns: *maxTurns,
		Model:    model,
		Verbose:  *verbose,
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s03] provider=%s model=%s tools=%d max_turns=%d\n",
			*provider, model, registry.Len(), *maxTurns)
	}

	final, err := loop.Run(context.Background(), prompt)
	if err != nil {
		log.Fatalf("loop error: %v", err)
	}
	fmt.Println(final)
}

// buildProvider resolves -provider/-base-url/-model into a Provider impl.
// Returns the resolved model id alongside so main can pass it to Loop.Model.
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
