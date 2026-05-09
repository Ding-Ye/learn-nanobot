package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubTool is a minimal Tool used to assert Registry behavior in isolation.
type stubTool struct {
	name string
	out  string
	err  error
}

func (s *stubTool) Schema() ToolSchema {
	return ToolSchema{Name: s.name, Description: "stub", InputSchema: map[string]interface{}{}}
}
func (s *stubTool) Execute(ctx context.Context, input map[string]interface{}) (string, error) {
	return s.out, s.err
}

func TestRegistry_Register_AppearsInDefinitions(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "alpha"})

	defs := r.Definitions()
	if len(defs) != 1 {
		t.Fatalf("expected 1 def, got %d", len(defs))
	}
	if defs[0].Name != "alpha" {
		t.Fatalf("got %q, want alpha", defs[0].Name)
	}
}

func TestRegistry_Definitions_CachedUntilMutation(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "alpha"})

	// Two consecutive calls must return the same slice header (cache hit).
	d1 := r.Definitions()
	d2 := r.Definitions()
	if &d1[0] != &d2[0] {
		t.Fatalf("expected cached slice — different backing arrays")
	}

	// Register invalidates.
	r.Register(&stubTool{name: "beta"})
	d3 := r.Definitions()
	if len(d3) != 2 {
		t.Fatalf("expected 2 after register, got %d", len(d3))
	}

	// Unregister also invalidates.
	r.Unregister("alpha")
	d4 := r.Definitions()
	if len(d4) != 1 || d4[0].Name != "beta" {
		t.Fatalf("expected only beta after unregister, got %+v", d4)
	}
}

func TestRegistry_BuiltinsBeforeMCP_StableOrder(t *testing.T) {
	r := NewRegistry()
	// Insert in scrambled order; expect builtins(name-sorted), then mcp(name-sorted).
	r.Register(&stubTool{name: "mcp_zoo"})
	r.Register(&stubTool{name: "bash"})
	r.Register(&stubTool{name: "mcp_aardvark"})
	r.Register(&stubTool{name: "alpha"})

	want := []string{"alpha", "bash", "mcp_aardvark", "mcp_zoo"}
	defs := r.Definitions()
	if len(defs) != len(want) {
		t.Fatalf("len mismatch: got %d, want %d", len(defs), len(want))
	}
	for i, w := range want {
		if defs[i].Name != w {
			t.Fatalf("at %d: got %q, want %q (full=%v)", i, defs[i].Name, w, names(defs))
		}
	}
}

func TestRegistry_Execute_UnknownTool_TypedError(t *testing.T) {
	r := NewRegistry()
	_, err := r.Execute(context.Background(), "ghost", nil)
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
}

func TestRegistry_Execute_ToolErrorBecomesContent(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "boom", err: errors.New("kaboom")})

	out, err := r.Execute(context.Background(), "boom", nil)
	if err != nil {
		t.Fatalf("execute should not propagate tool errors as Go errors, got %v", err)
	}
	if !strings.Contains(out, "tool error") || !strings.Contains(out, "kaboom") {
		t.Fatalf("expected stringified tool error, got %q", out)
	}
}

func TestRegistry_Has_And_Get(t *testing.T) {
	r := NewRegistry()
	r.Register(&stubTool{name: "alpha"})

	if !r.Has("alpha") {
		t.Fatalf("Has should be true")
	}
	if r.Has("missing") {
		t.Fatalf("Has should be false for unregistered")
	}
	t1, ok := r.Get("alpha")
	if !ok || t1.Schema().Name != "alpha" {
		t.Fatalf("Get returned %+v ok=%v", t1, ok)
	}
}

func names(defs []ToolSchema) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}
