package application

import (
	"context"
	"testing"

	"github.com/FastR-D/FastTask/internal/agent"
)

// stubTool is a minimal Tool for registry-level tests.
type stubTool struct {
	name   string
	level  ToolLevel
	params map[string]any
}

func (s stubTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: s.name, Description: "stub", Parameters: s.params}
}
func (s stubTool) Level() ToolLevel { return s.level }
func (s stubTool) Execute(context.Context, ToolContext, map[string]any) (ToolResult, error) {
	return ToolResult{Result: map[string]any{"ok": true}}, nil
}

// TestNewToolRegistryRejectsIdentityFields covers agent-impl.md §5.2 and §10
// ("工具参数里带 user_id 会被拒绝"): a tool whose schema declares an identity
// field cannot be registered at all — the ban is structural, not prompt-based.
func TestNewToolRegistryRejectsIdentityFields(t *testing.T) {
	for _, field := range []string{"user_id", "workspace_id", "actor", "tenant_id", "owner_id"} {
		tool := stubTool{name: "bad_" + field, params: objectSchema(map[string]any{
			field: stringProp("forbidden identity"),
		})}
		if _, err := NewToolRegistry(tool); err == nil {
			t.Fatalf("registry accepted a tool declaring identity field %q", field)
		}
	}
	// A nested identity field is also rejected.
	nested := stubTool{name: "nested", params: objectSchema(map[string]any{
		"filter": map[string]any{"type": "object", "properties": map[string]any{"user_id": stringProp("x")}},
	})}
	if _, err := NewToolRegistry(nested); err == nil {
		t.Fatal("registry accepted a nested identity field")
	}
}

// TestNewToolRegistryRejectsDuplicateNames asserts stable, unambiguous tool
// lookup (§5.1).
func TestNewToolRegistryRejectsDuplicateNames(t *testing.T) {
	a := stubTool{name: "dup", params: objectSchema(nil)}
	b := stubTool{name: "dup", params: objectSchema(nil)}
	if _, err := NewToolRegistry(a, b); err == nil {
		t.Fatal("registry accepted duplicate tool names")
	}
	if _, err := NewToolRegistry(stubTool{name: "", params: objectSchema(nil)}); err == nil {
		t.Fatal("registry accepted an empty tool name")
	}
}

// TestRegistryValidateArgsRejectsIdentityArguments covers §10: even if a model
// smuggles user_id as an argument (not in the schema), validation rejects it.
func TestRegistryValidateArgsRejectsIdentityArguments(t *testing.T) {
	registry, err := NewToolRegistry(stubTool{name: "list_goals", level: ToolReadonly, params: objectSchema(map[string]any{
		"status": stringProp("status"),
	})})
	if err != nil {
		t.Fatal(err)
	}
	problems := registry.ValidateArgs("list_goals", map[string]any{"status": "active", "user_id": "someone-else"})
	if len(problems) == 0 {
		t.Fatal("validation accepted a user_id argument")
	}
	foundIdentity, foundUnexpected := false, false
	for _, p := range problems {
		if contains(p, "forbidden") {
			foundIdentity = true
		}
		if contains(p, "unexpected argument") {
			foundUnexpected = true
		}
	}
	if !foundIdentity || !foundUnexpected {
		t.Fatalf("expected identity + additionalProperties rejection, got %v", problems)
	}
}

// TestRegistryValidateArgsSchema asserts the JSON Schema subset validator:
// required, type, enum, additionalProperties, integer bounds.
func TestRegistryValidateArgsSchema(t *testing.T) {
	registry, err := NewToolRegistry(stubTool{name: "get_task_tree", level: ToolReadonly, params: objectSchema(map[string]any{
		"goal_id": stringProp("goal"),
		"limit":   integerProp("limit", 1, 50),
		"mode":    stringProp("mode", "full", "compact"),
	}, "goal_id")})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		args        map[string]any
		wantProblem bool
	}{
		{"valid", map[string]any{"goal_id": "g1", "limit": float64(10), "mode": "full"}, false},
		{"missing required", map[string]any{"limit": float64(10)}, true},
		{"wrong type", map[string]any{"goal_id": float64(5)}, true},
		{"bad enum", map[string]any{"goal_id": "g1", "mode": "sideways"}, true},
		{"integer below min", map[string]any{"goal_id": "g1", "limit": float64(0)}, true},
		{"integer above max", map[string]any{"goal_id": "g1", "limit": float64(51)}, true},
		{"unexpected field", map[string]any{"goal_id": "g1", "bogus": "x"}, true},
		{"non-integer for integer type", map[string]any{"goal_id": "g1", "limit": 10.5}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := registry.ValidateArgs("get_task_tree", tc.args)
			if tc.wantProblem && len(problems) == 0 {
				t.Fatalf("expected a problem for %v", tc.args)
			}
			if !tc.wantProblem && len(problems) > 0 {
				t.Fatalf("unexpected problems %v for %v", problems, tc.args)
			}
		})
	}
	// Unknown tool is reported, not panicked.
	if problems := registry.ValidateArgs("nope", map[string]any{}); len(problems) == 0 {
		t.Fatal("unknown tool not reported")
	}
}

// TestRegistryDefinitionsFilterByLevel asserts the loop can advertise only the
// levels it supports this phase (§6): readonly now, proposal in phase D.
func TestRegistryDefinitionsFilterByLevel(t *testing.T) {
	registry, err := NewToolRegistry(
		stubTool{name: "ro", level: ToolReadonly, params: objectSchema(nil)},
		stubTool{name: "prop", level: ToolProposal, params: objectSchema(nil)},
	)
	if err != nil {
		t.Fatal(err)
	}
	readonly := registry.Definitions(ToolReadonly)
	if len(readonly) != 1 || readonly[0].Name != "ro" {
		t.Fatalf("readonly defs=%v", readonly)
	}
	all := registry.Definitions(ToolReadonly, ToolProposal)
	if len(all) != 2 {
		t.Fatalf("all defs=%v", all)
	}
	// Names are stable (sorted) so the advertised order is deterministic.
	names := registry.Names()
	if len(names) != 2 || names[0] != "prop" || names[1] != "ro" {
		t.Fatalf("names=%v, want sorted [prop ro]", names)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
