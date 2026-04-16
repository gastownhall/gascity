package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestOpenAPISpecServed verifies that the Huma-generated OpenAPI spec is
// accessible at /openapi.json and contains expected metadata.
func TestOpenAPISpecServed(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	// Huma serves the spec as application/openapi+json or application/json.
	if ct != "application/openapi+json" && ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/openapi+json or application/json", ct)
	}

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}

	// Check OpenAPI version.
	if v, ok := spec["openapi"].(string); !ok || v < "3.1" {
		t.Errorf("openapi version = %v, want >= 3.1", spec["openapi"])
	}

	// Check info.
	info, ok := spec["info"].(map[string]any)
	if !ok {
		t.Fatal("missing info in OpenAPI spec")
	}
	if title, _ := info["title"].(string); title != "Gas City API" {
		t.Errorf("info.title = %q, want %q", title, "Gas City API")
	}
}

// TestHumaHealthEndpoint verifies the Huma-migrated health endpoint returns
// the same JSON shape as the original handler.
func TestHumaHealthEndpoint(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("status = %v, want %q", resp["status"], "ok")
	}
	if resp["version"] != "test" {
		t.Errorf("version = %v, want %q", resp["version"], "test")
	}
	if resp["city"] != "test-city" {
		t.Errorf("city = %v, want %q", resp["city"], "test-city")
	}
	if _, ok := resp["uptime_sec"]; !ok {
		t.Error("missing uptime_sec in health response")
	}
}

// TestOpenAPISpecHasSignificantPaths verifies the spec contains a meaningful
// number of API paths, confirming the Huma migration is working.
func TestOpenAPISpecHasSignificantPaths(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode: %v", err)
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths in spec")
	}

	// Count total operations across all paths.
	var ops int
	for _, pathItem := range paths {
		if pi, ok := pathItem.(map[string]any); ok {
			for method := range pi {
				switch method {
				case "get", "post", "put", "patch", "delete":
					ops++
				}
			}
		}
	}

	t.Logf("OpenAPI spec: %d paths, %d operations", len(paths), ops)

	// We expect at least 50 operations from the Huma-migrated endpoints.
	if ops < 50 {
		t.Errorf("only %d operations in OpenAPI spec, expected >= 50", ops)
	}
}

// TestHumaHealthInOpenAPISpec verifies that the health endpoint appears
// in the auto-generated OpenAPI spec.
func TestHumaHealthInOpenAPISpec(t *testing.T) {
	state := newFakeState(t)
	srv := New(state)

	req := httptest.NewRequest("GET", "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths in OpenAPI spec")
	}

	healthPath, ok := paths["/health"]
	if !ok {
		t.Fatal("/health not found in OpenAPI spec paths")
	}

	healthOps, ok := healthPath.(map[string]any)
	if !ok {
		t.Fatal("/health path item is not an object")
	}

	if _, ok := healthOps["get"]; !ok {
		t.Error("GET operation not found for /health in OpenAPI spec")
	}
}
