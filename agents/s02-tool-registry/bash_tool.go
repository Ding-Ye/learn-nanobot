package main

import (
	"context"
	"fmt"
	"os/exec"
)

// BashTool — moved from s01's monolithic tools.go into its own file. As we
// add more built-in tools (s05+ will add read_file, web_fetch, etc.), this
// per-tool file pattern keeps the diffs small.

type BashTool struct{}

func NewBashTool() *BashTool { return &BashTool{} }

func (b *BashTool) Schema() ToolSchema {
	return ToolSchema{
		Name:        "bash",
		Description: "Run a shell command via /bin/bash -c and return combined stdout+stderr.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"command": map[string]interface{}{
					"type":        "string",
					"description": "The shell command to execute.",
				},
			},
			"required": []string{"command"},
		},
	}
}

func (b *BashTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	cmd, ok := input["command"].(string)
	if !ok {
		return "", fmt.Errorf("input.command must be a string, got %T", input["command"])
	}
	out, err := exec.CommandContext(ctx, "bash", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(exit error: %v)\n%s", err, string(out)), nil
	}
	return string(out), nil
}
