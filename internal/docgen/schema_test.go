package docgen

import (
	"encoding/json"
	"testing"
)

// defProperties extracts the properties map for a named $defs entry.
func defProperties(t *testing.T, raw map[string]interface{}, defName string) map[string]interface{} {
	t.Helper()
	defs, ok := raw["$defs"].(map[string]interface{})
	if !ok {
		t.Fatal("no $defs")
	}
	def, ok := defs[defName].(map[string]interface{})
	if !ok {
		t.Fatalf("no %s definition in $defs", defName)
	}
	props, ok := def["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s has no properties", defName)
	}
	return props
}

func TestGenerateCitySchema(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty schema output")
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// City properties are in $defs.City (schema uses $ref at top level).
	props := defProperties(t, raw, "City")
	for _, expected := range []string{"workspace", "providers", "agent", "rigs"} {
		if _, ok := props[expected]; !ok {
			t.Errorf("missing City property %q", expected)
		}
	}
	// Should NOT have Go-style names.
	for _, bad := range []string{"Workspace", "Providers", "Agents"} {
		if _, ok := props[bad]; ok {
			t.Errorf("found Go-style property %q, expected TOML name", bad)
		}
	}
}

func TestCitySchemaDescriptions(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Check that Agent fields have description from doc comments.
	agentProps := defProperties(t, raw, "Agent")
	nameField, ok := agentProps["name"].(map[string]interface{})
	if !ok {
		t.Fatal("Agent name property not a map")
	}
	desc, ok := nameField["description"].(string)
	if !ok || desc == "" {
		t.Error("Agent.name has no description — AddGoComments may not be extracting comments")
	}
}

func TestCitySchemaOrderOverrideIncludesLegacyGateAlias(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	props := defProperties(t, raw, "OrderOverride")
	gateField, ok := props["gate"].(map[string]interface{})
	if !ok {
		t.Fatal("OrderOverride.gate property missing from schema")
	}
	if deprecated, ok := gateField["deprecated"].(bool); !ok || !deprecated {
		t.Fatalf("OrderOverride.gate deprecated = %v, want true", gateField["deprecated"])
	}
}

func TestCitySchemaAgentDefinition(t *testing.T) {
	s, err := GenerateCitySchema()
	if err != nil {
		t.Fatalf("GenerateCitySchema: %v", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	agentProps := defProperties(t, raw, "Agent")

	// Check expected fields exist.
	for _, field := range []string{"name", "dir", "prompt_template", "provider", "pre_start"} {
		if _, ok := agentProps[field]; !ok {
			t.Errorf("Agent missing field %q", field)
		}
	}

	// Check pre_start is an array type.
	ps, ok := agentProps["pre_start"].(map[string]interface{})
	if !ok {
		t.Fatal("pre_start property not a map")
	}
	if ps["type"] != "array" {
		t.Errorf("pre_start type: got %v, want array", ps["type"])
	}

	// Check name is required.
	defs := raw["$defs"].(map[string]interface{})
	agent := defs["Agent"].(map[string]interface{})
	required, ok := agent["required"].([]interface{})
	if !ok {
		t.Fatal("Agent missing required array")
	}
	found := false
	for _, r := range required {
		if r == "name" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Agent 'name' not in required list")
	}
}
