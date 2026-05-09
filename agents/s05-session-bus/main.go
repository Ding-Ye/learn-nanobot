package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	maxTurns := flag.Int("max-turns", 20, "max agent iterations per turn before giving up")
	model := flag.String("model", envOr("MODEL", "claude-sonnet-4-6"),
		"Anthropic model id (default claude-sonnet-4-6)")
	sessionKey := flag.String("session", "cli:direct",
		"session key to send the message under (one CLI run = one session by default)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"usage: s05 [-max-turns N] [-model ID] [-session KEY] <prompt>\n\n"+
				"  ANTHROPIC_API_KEY must be set.\n\n"+
				"  s05 wires Runner behind a SessionManager + Bus. The CLI sends one\n"+
				"  inbound message under -session (default cli:direct) and waits for one\n"+
				"  outbound. Run multiple times with the same -session to keep history.\n"+
				"  Across processes there's no persistence yet — that's s06.\n")
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
	sm := NewSessionManager()
	bus := NewBus(sm, runner, registry, *model, *maxTurns)

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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
