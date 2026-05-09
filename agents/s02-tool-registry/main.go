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

// providerProfiles mirrors s01's profile menu. Each session re-declares it
// locally so sessions stay self-contained Go modules with no cross-imports.
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
			"usage: s02 [-v] [-max-turns N] [-provider P] [-base-url URL] [-model ID] <prompt>\n\n"+
				"  Same Provider abstraction as s01 (CreateMessage / Anthropic-flat\n"+
				"  wire format). The Tool Registry is what's new this chapter; the\n"+
				"  multi-provider plumbing is identical.\n")
	}
	flag.Parse()

	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	prompt := strings.Join(flag.Args(), " ")

	prof, ok := providerProfiles[*provider]
	if !ok {
		log.Fatalf("unknown -provider %q (valid: %s)", *provider, validProviderNames())
	}
	apiKey := os.Getenv(prof.APIKey)
	if apiKey == "" {
		log.Fatalf("%s is not set (required for -provider %s)", prof.APIKey, *provider)
	}
	model := *modelFlag
	if model == "" {
		model = prof.Model
	}
	url := *baseURL
	if url == "" {
		url = prof.BaseURL
	}

	var p Provider
	if *provider == "anthropic" {
		p = NewAnthropicProvider(apiKey, model)
	} else {
		p = NewOpenAIProvider(apiKey, url, model)
	}

	registry := NewRegistry()
	registry.Register(NewBashTool())

	loop := &Loop{
		Provider: p,
		Tools:    registry,
		MaxTurns: *maxTurns,
		Verbose:  *verbose,
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s02] provider=%s base_url=%q model=%s tools=%d max_turns=%d\n",
			*provider, url, model, registry.Len(), *maxTurns)
	}

	final, err := loop.Run(context.Background(), prompt)
	if err != nil {
		log.Fatalf("loop error: %v", err)
	}
	fmt.Println(final)
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
