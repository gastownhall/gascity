package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// TestOpenAPIHasNoMaintenanceSurface pins that the retired Dolt maintenance
// endpoints and the status fields that fed from them are absent from the
// served OpenAPI document.
func TestOpenAPIHasNoMaintenanceSurface(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d", rec.Code)
	}
	var spec struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	for path := range spec.Paths {
		if strings.Contains(path, "/maintenance/") {
			t.Errorf("OpenAPI still serves retired path %s", path)
		}
	}
	body := rec.Body.String()
	for _, field := range []string{`"last_gc_at"`, `"last_gc_status"`, "maintenance is overdue", "MaintenanceStatusBody", "MaintenanceRunBody"} {
		if strings.Contains(body, field) {
			t.Errorf("OpenAPI still mentions retired %s", field)
		}
	}
}
