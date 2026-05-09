package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	verbose := flag.Bool("v", false, "print every turn (assistant text + tool calls)")
	maxTurns := flag.Int("max-turns", 20, "max agent turns before giving up")
	model := flag.String("model", envOr("MODEL", "claude-sonnet-4-6"),
		"Anthropic model id (default claude-sonnet-4-6)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s01 [-v] [-max-turns N] [-model ID] <prompt>\n\n"+
				"  ANTHROPIC_API_KEY must be set.\n\n"+
				"  Examples:\n"+
				"    s01 \"hello\"                                 # one-shot\n"+
				"    s01 -v \"list .go files in this dir\"          # verbose, exercises bash tool\n"+
				"    s01 -model claude-haiku-4-5 -v \"echo hi\"\n\n"+
				"  Phase G adds an OpenAI-compatible provider so this CLI grows\n"+
				"  -provider / -base-url flags. Until then, s01 is Anthropic-only.\n")
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

	provider := NewAnthropicProvider(apiKey, *model)
	tools := []Tool{NewBashTool()}
	loop := &Loop{
		Provider: provider,
		Tools:    tools,
		MaxTurns: *maxTurns,
		Verbose:  *verbose,
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "[s01] model=%s max_turns=%d\n", *model, *maxTurns)
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
