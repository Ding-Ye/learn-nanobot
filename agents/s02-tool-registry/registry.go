package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Registry owns three jobs that s01's Loop did inline:
//   1. tool lookup by name
//   2. exporting JSON-schema definitions for the LLM prompt
//   3. dispatching execution
//
// The cache + sort logic mirrors nanobot/agent/tools/registry.py. Stable
// ordering (built-ins first, then MCP tools, both name-sorted) matters for
// LLM prompt-cache hit rate — same tools in same order = same prompt prefix.

type Registry struct {
	tools  map[string]Tool
	cached []ToolSchema
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// ErrUnknownTool is returned by Execute when the named tool isn't registered.
// The Loop catches this and writes a tool_result block — it does NOT abort
// the turn, because "the model asked for a non-existent tool" is a model
// cognition issue, not a loop failure.
var ErrUnknownTool = errors.New("unknown tool")

func (r *Registry) Register(t Tool) {
	r.tools[t.Schema().Name] = t
	r.cached = nil // invalidate
}

func (r *Registry) Unregister(name string) {
	delete(r.tools, name)
	r.cached = nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Has(name string) bool {
	_, ok := r.tools[name]
	return ok
}

func (r *Registry) Len() int { return len(r.tools) }

// Definitions returns the cached, stably-sorted list of tool schemas. The
// cache is invalidated by Register/Unregister.
//
// Order: built-in tools (name-sorted), then MCP tools (name-sorted, prefix
// "mcp_"). The Anthropic prompt cache hashes the prompt prefix; same order
// across turns = a cache hit on every iteration.
func (r *Registry) Definitions() []ToolSchema {
	if r.cached != nil {
		return r.cached
	}
	var builtins, mcp []ToolSchema
	for _, t := range r.tools {
		s := t.Schema()
		if strings.HasPrefix(s.Name, "mcp_") {
			mcp = append(mcp, s)
		} else {
			builtins = append(builtins, s)
		}
	}
	sort.Slice(builtins, func(i, j int) bool { return builtins[i].Name < builtins[j].Name })
	sort.Slice(mcp, func(i, j int) bool { return mcp[i].Name < mcp[j].Name })
	r.cached = append(builtins, mcp...)
	return r.cached
}

// Execute runs a tool by name and returns its output as a string. If the
// tool errors during execution, the error is converted to a string (so the
// model sees it as a tool_result, not as a loop crash). If the tool is
// unregistered, returns ErrUnknownTool — the Loop converts that to a
// tool_result without aborting.
func (r *Registry) Execute(ctx context.Context, name string, input map[string]interface{}) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	out, err := t.Execute(ctx, input)
	if err != nil {
		// Convert tool errors to content. Same rationale as s01: the model
		// can reason about "command failed" — propagating up would abort.
		return fmt.Sprintf("tool error: %v", err), nil
	}
	return out, nil
}
