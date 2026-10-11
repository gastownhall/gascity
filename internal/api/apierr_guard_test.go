package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
)

// TestErrorModelSpecProjection locks the two spec artifacts documentProblemTypes
// produces from the registry: the ErrorModel schema carries the machine `code`
// property, and the x-gascity-problem-types extension is exactly the sorted set
// of registered URNs. This is what keeps the published contract in lockstep with
// the catalog.
func TestErrorModelSpecProjection(t *testing.T) {
	sm := NewSupervisorMux(emptyRoundtripResolver{}, nil, false, "", "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d: %s", rec.Code, rec.Body.String())
	}

	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Extensions map[string]json.RawMessage `json:"-"`
					Examples   []any                      `json:"examples"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	errorModel, ok := spec.Components.Schemas["ErrorModel"]
	if !ok {
		t.Fatal("spec is missing the ErrorModel schema")
	}
	if _, ok := errorModel.Properties["code"]; !ok {
		t.Fatal("ErrorModel schema is missing the machine `code` property")
	}

	// x-gascity-problem-types must equal the sorted registry URNs. Re-parse the
	// raw type-property object to read the extension (Huma inlines x- extensions
	// as sibling keys on the schema object).
	var rawSpec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rawSpec); err != nil {
		t.Fatalf("parse spec (raw): %v", err)
	}
	typeProp := rawSpec.Components.Schemas["ErrorModel"].Properties["type"]
	got, _ := typeProp["x-gascity-problem-types"].([]any)
	var gotURNs []string
	for _, v := range got {
		if s, ok := v.(string); ok {
			gotURNs = append(gotURNs, s)
		}
	}

	var wantURNs []string
	for _, pt := range apierr.Registered() {
		wantURNs = append(wantURNs, pt.URN())
	}
	if !reflect.DeepEqual(gotURNs, wantURNs) {
		t.Fatalf("x-gascity-problem-types mismatch:\n got=%v\nwant=%v", gotURNs, wantURNs)
	}
}
