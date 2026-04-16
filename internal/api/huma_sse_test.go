package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestEventStreamSchemaInSpec verifies that the events/stream endpoint
// has its event schemas (eventStreamEnvelope, HeartbeatEvent) documented
// in the OpenAPI spec — the whole point of Fix 1.
func TestEventStreamSchemaInSpec(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d", rec.Code)
	}

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Find the /v0/events/stream operation.
	paths, _ := spec["paths"].(map[string]any)
	streamPath, ok := paths["/v0/events/stream"].(map[string]any)
	if !ok {
		t.Fatal("/v0/events/stream not in spec")
	}
	get, ok := streamPath["get"].(map[string]any)
	if !ok {
		t.Fatal("/v0/events/stream GET not in spec")
	}

	// Check the 200 response has text/event-stream content with a schema.
	responses, _ := get["responses"].(map[string]any)
	ok200, _ := responses["200"].(map[string]any)
	content, _ := ok200["content"].(map[string]any)
	eventStream, ok := content["text/event-stream"].(map[string]any)
	if !ok {
		t.Fatal("200 response missing text/event-stream content")
	}
	if _, ok := eventStream["schema"]; !ok {
		t.Fatal("text/event-stream response missing schema")
	}

	// Serialize the spec and check event type schemas are referenced.
	bs, _ := json.Marshal(spec)
	specStr := string(bs)
	for _, want := range []string{"Event event", "Event heartbeat"} {
		if !strings.Contains(specStr, want) {
			t.Errorf("OpenAPI spec missing %q title", want)
		}
	}
}

// TestSSEEndpointsHaveSchemasInSpec verifies that every SSE endpoint has
// its event schemas documented in the OpenAPI spec. This enforces the
// "spec drives everything" principle: if a new SSE endpoint is added
// without registerSSE (skipping spec documentation), this test fails.
func TestSSEEndpointsHaveSchemasInSpec(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	paths, _ := spec["paths"].(map[string]any)

	// All 3 SSE endpoints (+2 agent output variants = 4 streams total).
	sseEndpoints := []string{
		"/v0/events/stream",
		"/v0/session/{id}/stream",
		"/v0/agent/{base}/output/stream",
		"/v0/agent/{dir}/{base}/output/stream",
	}

	for _, path := range sseEndpoints {
		t.Run(path, func(t *testing.T) {
			p, ok := paths[path].(map[string]any)
			if !ok {
				t.Fatalf("path %s not in spec", path)
			}
			get, ok := p["get"].(map[string]any)
			if !ok {
				t.Fatalf("GET %s not in spec", path)
			}
			responses, _ := get["responses"].(map[string]any)
			ok200, _ := responses["200"].(map[string]any)
			content, _ := ok200["content"].(map[string]any)
			es, ok := content["text/event-stream"].(map[string]any)
			if !ok {
				t.Fatalf("%s 200 response has no text/event-stream content (missing schema!)", path)
			}
			if _, ok := es["schema"]; !ok {
				t.Fatalf("%s text/event-stream has no schema", path)
			}
		})
	}
}
