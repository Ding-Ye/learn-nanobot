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

// providerProfiles is s01's full menu of out-of-the-box providers. Eight is
// "covers the boring 80% without the user editing code":
//
//   - anthropic   — the original; native /v1/messages
//   - openai      — OpenAI's own Chat Completions
//   - deepseek    — DeepSeek (api.deepseek.com)
//   - moonshot    — Moonshot/Kimi (api.moonshot.cn)
//   - qwen        — Qwen via DashScope's compat endpoint
//   - groq        — Groq (cheap llama)
//   - openrouter  — OpenRouter (router over many models)
//   - local       — self-hosted vLLM/SGLang/Ollama on http://localhost:8000/v1
//
// Anything else is a single -base-url + -model away. The OpenAI-compat
// translator (provider_openai.go) handles all of them with one impl.
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
		"override the profile's base URL (e.g. for a custom OpenAI-compatible host)")
	modelFlag := flag.String("model", envOr("MODEL", ""),
		"override the profile's default model id")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s01 [-v] [-max-turns N] [-provider P] [-base-url URL] [-model ID] <prompt>\n\n"+
				"  Provider profiles (set -provider, then export the matching env var):\n"+
				"%s\n"+
				"  Examples:\n"+
				"    s01 \"hello\"                                            # default: anthropic\n"+
				"    s01 -provider deepseek -v \"list the .go files here\"\n"+
				"    s01 -provider qwen \"周末天气如何？\"\n"+
				"    s01 -provider local -base-url http://127.0.0.1:8000/v1 -model my-llama \"hi\"\n\n"+
				"  Anthropic uses the native /v1/messages API. Every other profile uses\n"+
				"  the OpenAI-compatible Chat Completions API via provider_openai.go.\n",
			renderProviderTable())
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

	tools := []Tool{NewBashTool()}
	loop := &Loop{
		Provider: p,
		Tools:    tools,
		MaxTurns: *maxTurns,
		Verbose:  *verbose,
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s01] provider=%s base_url=%q model=%s max_turns=%d\n",
			*provider, url, model, *maxTurns)
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

func renderProviderTable() string {
	names := make([]string, 0, len(providerProfiles))
	for k := range providerProfiles {
		names = append(names, k)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, name := range names {
		p := providerProfiles[name]
		base := p.BaseURL
		if base == "" {
			base = "(native /v1/messages)"
		}
		fmt.Fprintf(&sb, "    %-12s env=%-22s model=%s\n", name, p.APIKey, p.Model)
		fmt.Fprintf(&sb, "    %-12s base=%s\n", "", base)
	}
	return sb.String()
}
