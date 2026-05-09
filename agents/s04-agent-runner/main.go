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
	verbose := flag.Bool("v", false, "print every iteration (assistant text + tool calls)")
	maxTurns := flag.Int("max-turns", 20, "max agent iterations before giving up")
	model := flag.String("model", envOr("MODEL", "claude-sonnet-4-6"),
		"Anthropic model id (default claude-sonnet-4-6)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s04 [-v] [-max-turns N] [-model ID] <prompt>\n\n"+
				"  ANTHROPIC_API_KEY must be set.\n\n"+
				"  s04 splits the inner loop into a Runner that takes a structured\n"+
				"  AgentRunSpec (initial messages built by the caller) and returns\n"+
				"  an AgentRunResult (final content + messages + tools_used + usage).\n"+
				"  The s03 Loop has been narrowed; s11 will wrap it in an outer\n"+
				"  state machine that's named Loop again.\n")
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
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(provider)

	if *verbose {
		fmt.Fprintf(os.Stderr, "[s04] model=%s tools=%d max_iterations=%d\n",
			*model, registry.Len(), *maxTurns)
	}

	// Caller builds the initial messages — that's the new seam.
	// In s08 the ContextBuilder takes over this step.
	spec := AgentRunSpec{
		InitialMessages: []Message{{
			Role:    "user",
			Content: []ContentBlock{{Type: "text", Text: prompt}},
		}},
		Tools:         registry,
		Model:         *model,
		MaxIterations: *maxTurns,
		Verbose:       *verbose,
	}

	result, err := runner.Run(context.Background(), spec)
	if err != nil {
		log.Fatalf("runner error: %v", err)
	}

	fmt.Println(result.FinalContent)

	if *verbose {
		fmt.Fprintf(os.Stderr, "[s04] tools_used=%v input_tokens=%d output_tokens=%d finish=%s\n",
			result.ToolsUsed, result.Usage.InputTokens, result.Usage.OutputTokens, result.FinishReason)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
