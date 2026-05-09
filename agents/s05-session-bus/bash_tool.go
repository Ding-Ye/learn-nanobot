package main

import (
	"context"
	"fmt"
	"os/exec"
)

// BashTool — same as s02/s03/s04. The only built-in tool we ship in s05.

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
