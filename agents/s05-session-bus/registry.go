package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Registry — same as s02/s03/s04. The Runner consumes *Registry; tool
// plumbing lives entirely behind these methods.

type Registry struct {
	tools  map[string]Tool
	cached []ToolSchema
}

func NewRegistry() *Registry { return &Registry{tools: map[string]Tool{}} }

var ErrUnknownTool = errors.New("unknown tool")

func (r *Registry) Register(t Tool) {
	r.tools[t.Schema().Name] = t
	r.cached = nil
}

func (r *Registry) Unregister(name string) {
	delete(r.tools, name)
	r.cached = nil
}

func (r *Registry) Get(name string) (Tool, bool) { t, ok := r.tools[name]; return t, ok }
func (r *Registry) Has(name string) bool         { _, ok := r.tools[name]; return ok }
func (r *Registry) Len() int                     { return len(r.tools) }

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

func (r *Registry) Execute(ctx context.Context, name string, input map[string]interface{}) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	out, err := t.Execute(ctx, input)
	if err != nil {
		return fmt.Sprintf("tool error: %v", err), nil
	}
	return out, nil
}
