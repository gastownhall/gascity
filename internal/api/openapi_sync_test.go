package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// TestOpenAPISpecInSync enforces that the committed openapi.json file
// matches the spec the supervisor actually serves. If this test fails,
// regenerate the spec via:
//
//	go run ./cmd/genspec
//
// The supervisor is the single Huma API; a GET /openapi.json against it
// yields the authoritative contract for every HTTP endpoint the control
// plane exposes.
func TestOpenAPISpecInSync(t *testing.T) {
	sm := api.NewSupervisorMux(emptyTestResolver{}, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json returned %d: %s", rec.Code, rec.Body.String())
	}

	var live any
	if err := json.Unmarshal(rec.Body.Bytes(), &live); err != nil {
		t.Fatalf("parse live spec: %v", err)
	}
	var liveBuf bytes.Buffer
	enc := json.NewEncoder(&liveBuf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(live); err != nil {
		t.Fatalf("encode live spec: %v", err)
	}

	specPath := filepath.Join("openapi.json")
	onDisk, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v (run `go run ./cmd/genspec` to create it)", specPath, err)
	}

	if !bytes.Equal(onDisk, liveBuf.Bytes()) {
		t.Fatalf("openapi.json is out of sync with the live server spec.\n"+
			"Run `go run ./cmd/genspec` to regenerate.\n"+
			"Live spec size: %d bytes, on-disk size: %d bytes",
			liveBuf.Len(), len(onDisk))
	}
}

// emptyTestResolver is a CityResolver with no cities. Huma schema
// generation is reflection-based and never calls resolver methods.
type emptyTestResolver struct{}

func (emptyTestResolver) ListCities() []api.CityInfo      { return nil }
func (emptyTestResolver) CityState(name string) api.State { return nil }
