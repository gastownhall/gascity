// Command genspec writes the live OpenAPI 3.1 spec to disk so downstream
// clients (CLI, dashboard, third-party consumers) can be generated from
// it. The supervisor's Huma API owns every operation, so we fetch
// /openapi.json directly from a supervisor constructed against an empty
// resolver — no merge step, no per-city spec to combine, one
// authoritative source of truth.
//
// Usage:
//
//	go run ./cmd/genspec > internal/api/openapi.json
//
// If this output drifts from what the running supervisor serves,
// TestOpenAPISpecInSync fails.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

func main() {
	sm := api.NewSupervisorMux(emptyResolver{}, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		fmt.Fprintf(os.Stderr, "GET /openapi.json returned %d: %s\n", rec.Code, rec.Body.String())
		os.Exit(1)
	}

	// Pretty-print for a stable, reviewable diff.
	var raw any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		fmt.Fprintf(os.Stderr, "parse spec: %v\n", err)
		os.Exit(1)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(raw); err != nil {
		fmt.Fprintf(os.Stderr, "encode spec: %v\n", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}
}

// emptyResolver implements api.CityResolver with no cities. Schema
// generation is reflection-based and never calls resolver methods.
type emptyResolver struct{}

func (emptyResolver) ListCities() []api.CityInfo      { return nil }
func (emptyResolver) CityState(name string) api.State { return nil }
