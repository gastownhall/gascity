// Command gen-client generates the typed Go API client from the live
// OpenAPI spec.
//
// Pipeline:
//  1. Fetch the 3.0-downgrade spec directly from a SupervisorMux built
//     against an empty resolver. Huma v2 emits the downgrade
//     automatically; oapi-codegen v2.6.0 consumes it cleanly where it
//     chokes on 3.1. The supervisor owns every operation, so one fetch
//     yields the entire API surface — no merge step.
//  2. Pipe the spec unchanged to oapi-codegen. There is NO preprocessing.
//     The routes we register ARE the routes we expose. Every schema and
//     path in the generated client matches what the server publishes to
//     external consumers — no hidden rename, no hidden path rewrite.
//     skip-prune keeps documentation-only compatibility components
//     available to in-tree callers that still deserialize those shapes.
//  3. Write the generated client to internal/api/genclient/client_gen.go.
//
// Usage:
//
//	go run ./cmd/gen-client > internal/api/genclient/client_gen.go
//
// Or via go:generate in internal/api/genclient/doc.go. A CI drift test
// regenerates the client and diffs against the committed file so the
// spec is the source of truth.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// Step 1: fetch the 3.0-downgraded spec from the supervisor.
	sm := api.NewSupervisorMux(emptyResolver{}, nil, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi-3.0.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /openapi-3.0.json returned %d: %s", rec.Code, rec.Body.String())
	}

	// Step 2: write the spec verbatim to a temp file for oapi-codegen.
	tmp, err := os.CreateTemp("", "gc-openapi-3.0-*.json")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(rec.Body.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp spec: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp spec: %w", err)
	}

	// Step 3: invoke oapi-codegen. Output goes to stdout — the caller
	// redirects it to internal/api/genclient/client_gen.go.
	cmd := exec.Command("oapi-codegen", "-generate", "types,client,skip-prune", "-package", "genclient", tmp.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("oapi-codegen: %w", err)
	}
	return nil
}

// emptyResolver implements api.CityResolver with no cities. Schema
// generation is reflection-based and never calls resolver methods.
type emptyResolver struct{}

func (emptyResolver) ListCities() []api.CityInfo   { return nil }
func (emptyResolver) CityState(_ string) api.State { return nil }
