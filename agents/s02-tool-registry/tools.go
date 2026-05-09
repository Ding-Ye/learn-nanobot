package main

import "context"

// Tool — same shape as s01. Concrete tools (BashTool, future MCP wrappers,
// future Skill-as-Tool) all implement this. The Loop never sees a concrete
// tool — only the Registry does.
type Tool interface {
	Schema() ToolSchema
	Execute(ctx context.Context, input map[string]interface{}) (string, error)
}
