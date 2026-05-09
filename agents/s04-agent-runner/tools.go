package main

import "context"

// Tool — same interface as s02/s03. Re-defined here because each session
// is its own self-contained Go module.
type Tool interface {
	Schema() ToolSchema
	Execute(ctx context.Context, input map[string]interface{}) (string, error)
}
