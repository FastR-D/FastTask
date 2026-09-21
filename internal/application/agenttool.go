package application

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/FastR-D/FastTask/internal/agent"
)

// ToolLevel classifies a tool's write authority (doc/agent.md §5, ADR-0003 §2).
type ToolLevel string

const (
	// ToolReadonly executes directly; its result re-enters the model context. It
	// runs OUTSIDE any database transaction (§5.2) and is always user-scoped.
	ToolReadonly ToolLevel = "readonly"
	// ToolProposal produces a Proposal for user approval and never touches
	// business tables (§5.2). Landing in phase D.
	ToolProposal ToolLevel = "proposal"
)

// ToolContext carries the authenticated identity and run coordinates into a tool
// execution. UserID ALWAYS comes from here (the auth context), never from model
// arguments — invariant 5 (agent.md §4, agent-impl.md §5.2).
type ToolContext struct {
	UserID   string
	ThreadID string
	RunID    string
	// ActiveGoalID is an optional hint from the thread binding; tools must still
	// scope every query by UserID.
	ActiveGoalID string
}

// ToolResult is the outcome of a tool execution. Exactly one of Result or
// IsError is meaningful: on error, Text is a structured message fed back to the
// model so it can self-correct (§5.2) rather than failing the whole run.
type ToolResult struct {
	Result  any
	IsError bool
	Text    string
}

// Tool is a controlled projection of an application use case (§5). The Execute
// function receives validated arguments and the identity-bearing ToolContext.
type Tool interface {
	Definition() agent.ToolDefinition
	Level() ToolLevel
	Execute(ctx context.Context, tc ToolContext, args map[string]any) (ToolResult, error)
}

// identityArgNames are argument keys that must never appear in a tool schema or
// a model-supplied argument. Their presence is an implementation error (§5.2):
// identity is contextual, so a model must not be able to assert it.
var identityArgNames = map[string]bool{
	"user_id": true, "userid": true, "user": true, "workspace_id": true,
	"workspaceid": true, "workspace": true, "account_id": true, "actor": true,
	"actor_id": true, "principal": true, "identity": true, "tenant": true,
	"tenant_id": true, "owner_id": true, "on_behalf_of": true,
}

// ToolRegistry holds the tools the loop may advertise and execute. It is built
// from tools contributed by application services (fx value group agent_tools,
// wiring.md §5), never enumerated in a central switch.
type ToolRegistry struct {
	tools map[string]Tool
	order []string
}

// NewToolRegistry builds a registry, rejecting duplicate names and any tool whose
// schema leaks an identity field (a construction-time invariant, §5.2).
func NewToolRegistry(tools ...Tool) (*ToolRegistry, error) {
	registry := &ToolRegistry{tools: map[string]Tool{}}
	for _, tool := range tools {
		def := tool.Definition()
		name := strings.TrimSpace(def.Name)
		if name == "" {
			return nil, fmt.Errorf("tool with empty name")
		}
		if _, exists := registry.tools[name]; exists {
			return nil, fmt.Errorf("duplicate tool name %q", name)
		}
		if err := assertNoIdentityFields(name, def.Parameters); err != nil {
			return nil, err
		}
		registry.tools[name] = tool
		registry.order = append(registry.order, name)
	}
	sort.Strings(registry.order)
	return registry, nil
}

// Names returns the registered tool names in stable order.
func (r *ToolRegistry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.order...)
}

// Get returns a tool by name.
func (r *ToolRegistry) Get(name string) (Tool, bool) {
	if r == nil {
		return nil, false
	}
	tool, ok := r.tools[name]
	return tool, ok
}

// Definitions returns the wire schemas for tools at the given levels, in stable
// order. The loop advertises readonly tools always, and proposal tools once
// phase D lands (§6).
func (r *ToolRegistry) Definitions(levels ...ToolLevel) []agent.ToolDefinition {
	if r == nil {
		return nil
	}
	allow := map[ToolLevel]bool{}
	for _, level := range levels {
		allow[level] = true
	}
	defs := make([]agent.ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		tool := r.tools[name]
		if len(allow) > 0 && !allow[tool.Level()] {
			continue
		}
		defs = append(defs, tool.Definition())
	}
	return defs
}

// ValidateArgs schema-validates model-supplied arguments and enforces the
// identity-field ban. It returns a human-readable error list; an empty slice
// means valid. Model output is untrusted (arch.md §12), so this runs before any
// domain call (§5.2).
func (r *ToolRegistry) ValidateArgs(name string, args map[string]any) []string {
	tool, ok := r.Get(name)
	if !ok {
		return []string{fmt.Sprintf("unknown tool %q", name)}
	}
	var problems []string
	for key := range args {
		if identityArgNames[strings.ToLower(strings.TrimSpace(key))] {
			problems = append(problems, fmt.Sprintf("argument %q is forbidden: identity comes from the authenticated context, not tool arguments", key))
		}
	}
	problems = append(problems, validateAgainstSchema(tool.Definition().Parameters, args)...)
	sort.Strings(problems)
	return problems
}

// assertNoIdentityFields walks a JSON Schema and fails if any property name is an
// identity field. This is a construction-time guard so a tool that accidentally
// declares user_id cannot be registered at all (§5.2).
func assertNoIdentityFields(toolName string, schema map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	for key, prop := range props {
		if identityArgNames[strings.ToLower(strings.TrimSpace(key))] {
			return fmt.Errorf("tool %q declares identity field %q; identity must come from context", toolName, key)
		}
		if nested, ok := prop.(map[string]any); ok {
			if err := assertNoIdentityFields(toolName, nested); err != nil {
				return err
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		if err := assertNoIdentityFields(toolName, items); err != nil {
			return err
		}
	}
	return nil
}

// validateAgainstSchema is a focused JSON Schema validator covering the subset
// the tool schemas use: type, required, properties, additionalProperties, enum,
// items, minimum/maximum. It intentionally has no external dependency and
// returns readable messages so the model can self-correct (§5.2, §6).
func validateAgainstSchema(schema map[string]any, value map[string]any) []string {
	if schema == nil {
		return nil
	}
	var problems []string

	// required
	if required, ok := schema["required"].([]any); ok {
		for _, item := range required {
			key, _ := item.(string)
			if key == "" {
				continue
			}
			if _, present := value[key]; !present {
				problems = append(problems, fmt.Sprintf("missing required argument %q", key))
			}
		}
	}

	props, _ := schema["properties"].(map[string]any)
	additional, hasAdditional := schema["additionalProperties"]
	additionalAllowed := true
	if hasAdditional {
		if b, ok := additional.(bool); ok {
			additionalAllowed = b
		}
	}
	for key, val := range value {
		propSchema, declared := props[key]
		if !declared {
			if !additionalAllowed {
				problems = append(problems, fmt.Sprintf("unexpected argument %q", key))
			}
			continue
		}
		pm, _ := propSchema.(map[string]any)
		problems = append(problems, validateValue(key, pm, val)...)
	}
	sort.Strings(problems)
	return problems
}

func validateValue(path string, schema map[string]any, value any) []string {
	if schema == nil {
		return nil
	}
	// A JSON null satisfies any optional field.
	if value == nil {
		return nil
	}
	var problems []string
	if expected, ok := schema["type"].(string); ok && !typeMatches(expected, value) {
		return append(problems, fmt.Sprintf("argument %q must be of type %s", path, expected))
	}
	if enum, ok := schema["enum"].([]any); ok {
		if !containsValue(enum, value) {
			problems = append(problems, fmt.Sprintf("argument %q must be one of %v", path, enum))
		}
	}
	switch typed := value.(type) {
	case float64:
		if min, ok := numberFrom(schema["minimum"]); ok && typed < min {
			problems = append(problems, fmt.Sprintf("argument %q must be >= %v", path, schema["minimum"]))
		}
		if max, ok := numberFrom(schema["maximum"]); ok && typed > max {
			problems = append(problems, fmt.Sprintf("argument %q must be <= %v", path, schema["maximum"]))
		}
	case map[string]any:
		problems = append(problems, validateAgainstSchema(schema, typed)...)
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, element := range typed {
				if em, ok := element.(map[string]any); ok {
					problems = append(problems, validateValue(fmt.Sprintf("%s[%d]", path, i), items, em)...)
				} else if !typeMatches(schemaTypeOf(items), element) && schemaTypeOf(items) != "" {
					problems = append(problems, fmt.Sprintf("argument %s[%d] must be of type %s", path, i, schemaTypeOf(items)))
				}
			}
		}
	}
	return problems
}

func schemaTypeOf(schema map[string]any) string {
	t, _ := schema["type"].(string)
	return t
}

func typeMatches(expected string, value any) bool {
	switch expected {
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		// JSON numbers decode as float64; an integer is a float64 with no fraction.
		number, ok := value.(float64)
		return ok && number == float64(int64(number))
	case "number":
		_, ok := value.(float64)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "":
		return true
	default:
		return true
	}
}

func containsValue(enum []any, value any) bool {
	for _, item := range enum {
		if fmt.Sprint(item) == fmt.Sprint(value) {
			return true
		}
	}
	return false
}

func numberFrom(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

// objectSchema is a small builder for the tool parameter schemas so definitions
// stay readable and always set additionalProperties:false (which, with the
// identity guard, blocks a model from smuggling user_id).
func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		list := make([]any, 0, len(required))
		for _, item := range required {
			list = append(list, item)
		}
		schema["required"] = list
	}
	return schema
}

func stringProp(description string, enum ...string) map[string]any {
	prop := map[string]any{"type": "string", "description": description}
	if len(enum) > 0 {
		list := make([]any, 0, len(enum))
		for _, item := range enum {
			list = append(list, item)
		}
		prop["enum"] = list
	}
	return prop
}

func integerProp(description string, min, max int) map[string]any {
	prop := map[string]any{"type": "integer", "description": description}
	if min != 0 || max != 0 {
		prop["minimum"] = float64(min)
		prop["maximum"] = float64(max)
	}
	return prop
}
