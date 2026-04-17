package api

import (
	"testing"
)

// TestAgentCreateSpecMarksFieldsRequired verifies that the OpenAPI spec
// marks name and provider as required fields (Phase 2 Fix 2: no more
// omitempty bypass hiding required fields).
func TestAgentCreateSpecMarksFieldsRequired(t *testing.T) {
	spec := readCommittedOpenAPISpec(t)

	// Walk to the request body schema for POST /v0/agents (still
	// per-city during the transition).
	paths, _ := spec["paths"].(map[string]any)
	agentsPath, _ := paths["/v0/agents"].(map[string]any)
	post, _ := agentsPath["post"].(map[string]any)
	reqBody, _ := post["requestBody"].(map[string]any)
	content, _ := reqBody["content"].(map[string]any)
	appJSON, _ := content["application/json"].(map[string]any)
	schema, _ := appJSON["schema"].(map[string]any)

	// Schema is usually a $ref; resolve it.
	if ref, ok := schema["$ref"].(string); ok {
		// "#/components/schemas/FooRequest" → FooRequest
		name := ref[len("#/components/schemas/"):]
		components, _ := spec["components"].(map[string]any)
		schemas, _ := components["schemas"].(map[string]any)
		resolved, ok := schemas[name].(map[string]any)
		if !ok {
			t.Fatalf("could not resolve $ref %s", ref)
		}
		schema = resolved
	}

	required, _ := schema["required"].([]any)
	reqMap := make(map[string]bool)
	for _, r := range required {
		if s, ok := r.(string); ok {
			reqMap[s] = true
		}
	}

	if !reqMap["name"] {
		t.Errorf("agent create schema does not mark name as required; required=%v", required)
	}
	if !reqMap["provider"] {
		t.Errorf("agent create schema does not mark provider as required; required=%v", required)
	}
}
