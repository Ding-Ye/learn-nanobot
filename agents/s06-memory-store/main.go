package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	maxTurns := flag.Int("max-turns", 20, "max agent iterations per turn before giving up")
	model := flag.String("model", envOr("MODEL", "claude-sonnet-4-6"),
		"Anthropic model id (default claude-sonnet-4-6)")
	sessionKey := flag.String("session", "cli:direct",
		"session key to send the message under (one CLI run = one session by default)")
	workspace := flag.String("workspace", "",
		"workspace root directory (default ~/.learn-nanobot/agents/default)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s06 [-max-turns N] [-model ID] [-session KEY] [-workspace DIR] <prompt>\n\n"+
				"  ANTHROPIC_API_KEY must be set.\n\n"+
				"  s06 wires Runner + Bus + a real on-disk MemoryStore. Each turn is\n"+
				"  appended to <workspace>/history.jsonl and the cursor lands in\n"+
				"  <workspace>/.cursor. MEMORY.md / SOUL.md / USER.md are read on demand;\n"+
				"  s10 (Consolidator) and App. A (Dream) are the writers.\n")
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

	provider := NewAnthropicProvider(apiKey, *model)
	registry := NewRegistry()
	registry.Register(NewBashTool())
	runner := NewRunner(provider)
	sm := NewSessionManager()
	bus := NewBus(sm, runner, registry, *model, *maxTurns)

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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
