package api

import (
	"net/http"
	"strconv"
	"testing"
)

// TestAgentCreateSpecMarksFieldsRequired verifies that the OpenAPI spec
// marks name and provider as required fields (Phase 2 Fix 2: no more
// omitempty bypass hiding required fields).
func TestAgentCreateSpecMarksFieldsRequired(t *testing.T) {
	spec := readCommittedOpenAPISpec(t)

	// Walk to the request body schema for POST /v0/city/{cityName}/agents.
	paths, _ := spec["paths"].(map[string]any)
	agentsPath, _ := paths["/v0/city/{cityName}/agents"].(map[string]any)
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

func TestAgentSuspensionSpecIsCollisionFreeAndComplete(t *testing.T) {
	spec := readCommittedOpenAPISpec(t)
	paths, _ := spec["paths"].(map[string]any)
	for _, obsolete := range []string{
		"/v0/city/{cityName}/agent/{base}/suspension",
		"/v0/city/{cityName}/agent/{dir}/{base}/suspension",
	} {
		if _, exists := paths[obsolete]; exists {
			t.Errorf("collision-prone CAS path %q is still published", obsolete)
		}
	}

	for _, path := range []string{
		"/v0/city/{cityName}/agent-suspension/{base}",
		"/v0/city/{cityName}/agent-suspension/{dir}/{base}",
	} {
		pathItem, ok := paths[path].(map[string]any)
		if !ok {
			t.Fatalf("CAS path %q is missing", path)
		}
		for method, statuses := range map[string][]int{
			"get": {http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden},
			"put": {http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge},
		} {
			op, ok := pathItem[method].(map[string]any)
			if !ok {
				t.Errorf("%s %s is missing", method, path)
				continue
			}
			responses, _ := op["responses"].(map[string]any)
			for _, status := range statuses {
				statusKey := strconv.Itoa(status)
				if _, exists := responses[statusKey]; !exists {
					t.Errorf("%s %s does not declare response %d", method, path, status)
				}
			}
		}
	}

	components, _ := spec["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	stateSchema, _ := schemas["AgentSuspensionStateBody"].(map[string]any)
	properties, _ := stateSchema["properties"].(map[string]any)
	token, ok := properties["token"].(map[string]any)
	if !ok {
		t.Fatal("AgentSuspensionStateBody.token schema is missing")
	}
	if token["minLength"] != float64(64) || token["maxLength"] != float64(64) || token["pattern"] != "^[0-9a-f]{64}$" {
		t.Fatalf("token schema = %#v, want exactly 64 lowercase hexadecimal characters", token)
	}
}
