package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// TestSessionConfigDriftDeferredPayloadContract pins the typed-wire contract
// of the session.config_drift_deferred payload (#8345): the generated spec
// declares drifted_fields required and a plain, non-nullable array — the
// constructor never marshals null, so a nullable schema would lie about the
// wire — and the wire form itself carries a non-null drifted_fields even for
// a session whose per-field breakdown was unavailable.
func TestSessionConfigDriftDeferredPayloadContract(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, nil, false, "", "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}

	var spec struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("parse live spec: %v", err)
	}
	schema, ok := spec.Components.Schemas["SessionConfigDriftDeferredPayload"]
	if !ok {
		t.Fatal("SessionConfigDriftDeferredPayload missing from the generated spec")
	}
	var payloadSchema struct {
		Properties map[string]struct {
			Type json.RawMessage `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &payloadSchema); err != nil {
		t.Fatalf("parse SessionConfigDriftDeferredPayload schema: %v", err)
	}

	var fieldsType any
	if err := json.Unmarshal(payloadSchema.Properties["drifted_fields"].Type, &fieldsType); err != nil {
		t.Fatalf("parse drifted_fields type: %v", err)
	}
	switch typ := fieldsType.(type) {
	case string:
		if typ != "array" {
			t.Fatalf("drifted_fields type = %q, want array", typ)
		}
	case []any:
		t.Fatalf("drifted_fields type = %v, want a plain non-nullable array", fieldsType)
	}

	var required bool
	for _, name := range payloadSchema.Required {
		required = required || name == "drifted_fields"
	}
	if !required {
		t.Fatalf("drifted_fields missing from required %v", payloadSchema.Required)
	}

	raw := api.SessionConfigDriftDeferredPayloadJSON("gc-1", "s-worker", "worker", nil, "gw-1", time.Time{}, time.Time{}, false)
	var wire struct {
		DriftedFields json.RawMessage `json:"drifted_fields"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse wire payload %s: %v", raw, err)
	}
	if len(wire.DriftedFields) == 0 || string(wire.DriftedFields) == "null" {
		t.Fatalf("wire drifted_fields = %s, want a non-null array", wire.DriftedFields)
	}
}
