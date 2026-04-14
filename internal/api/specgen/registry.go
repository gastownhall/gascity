// Package specgen provides auto-generation of AsyncAPI and OpenAPI specs
// from the Go types used by the WebSocket and HTTP API.
//
// The action registry is the single source of truth — every WS action
// maps to its request/response Go types. The generator reflects on these
// types to produce JSON Schema, then assembles the full spec YAML.
//
// Usage:
//
//	go run ./cmd/specgen
//	go generate ./internal/api/specgen/...
//
//go:generate go run ../../cmd/specgen
package specgen

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// ActionDef describes a single WebSocket action's request and response types.
type ActionDef struct {
	// Action is the dotted action name (e.g., "bead.create").
	Action string

	// Description is a short human-readable description.
	Description string

	// RequestType is the Go type for the request payload. Nil means no payload.
	RequestType reflect.Type

	// ResponseType is the Go type for the response result. Nil means no result.
	ResponseType reflect.Type

	// IsMutation is true if the action modifies state.
	IsMutation bool
}

// Registry holds all registered WS action definitions.
type Registry struct {
	actions []ActionDef
}

// NewRegistry creates a new empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds an action definition.
func (r *Registry) Register(def ActionDef) {
	r.actions = append(r.actions, def)
}

// Actions returns all registered actions sorted by name.
func (r *Registry) Actions() []ActionDef {
	sorted := make([]ActionDef, len(r.actions))
	copy(sorted, r.actions)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Action < sorted[j].Action
	})
	return sorted
}

// ActionNames returns sorted action names for the AsyncAPI enum.
func (r *Registry) ActionNames() []string {
	actions := r.Actions()
	names := make([]string, len(actions))
	for i, a := range actions {
		names[i] = a.Action
	}
	return names
}

// JSONSchema generates a JSON Schema object from a Go type using struct tags.
func JSONSchema(t reflect.Type) map[string]any {
	if t == nil {
		return nil
	}
	// Dereference pointer types.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		return structSchema(t)
	case reflect.Slice:
		return map[string]any{
			"type":  "array",
			"items": JSONSchema(t.Elem()),
		}
	case reflect.Map:
		return map[string]any{
			"type":                 "object",
			"additionalProperties": JSONSchema(t.Elem()),
		}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Interface:
		// any / interface{} — no schema constraint
		return map[string]any{}
	default:
		return map[string]any{}
	}
}

func structSchema(t reflect.Type) map[string]any {
	// Special case: json.RawMessage is just raw JSON.
	if t == reflect.TypeOf(json.RawMessage{}) {
		return map[string]any{}
	}

	properties := map[string]any{}
	var required []string

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		// Handle embedded structs (promoted fields).
		if field.Anonymous {
			embedded := JSONSchema(field.Type)
			if props, ok := embedded["properties"].(map[string]any); ok {
				for k, v := range props {
					properties[k] = v
				}
			}
			if req, ok := embedded["required"].([]string); ok {
				required = append(required, req...)
			}
			continue
		}

		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}

		name, opts := parseJSONTag(tag)
		if name == "" {
			name = field.Name
		}

		fieldSchema := JSONSchema(field.Type)

		// Add description from "description" struct tag if present.
		if desc := field.Tag.Get("description"); desc != "" {
			fieldSchema["description"] = desc
		}

		properties[name] = fieldSchema

		// Fields without omitempty are required.
		if !opts.Contains("omitempty") {
			required = append(required, name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		sort.Strings(required)
		schema["required"] = required
	}
	return schema
}

type tagOptions string

func (o tagOptions) Contains(name string) bool {
	for o != "" {
		var next string
		i := strings.Index(string(o), ",")
		if i >= 0 {
			next = string(o[i+1:])
			o = o[:i]
		} else {
			next = ""
		}
		if string(o) == name {
			return true
		}
		o = tagOptions(next)
	}
	return false
}

func parseJSONTag(tag string) (string, tagOptions) {
	if idx := strings.Index(tag, ","); idx >= 0 {
		return tag[:idx], tagOptions(tag[idx+1:])
	}
	return tag, ""
}
