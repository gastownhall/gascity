// Command genspec writes the live OpenAPI 3.1 spec to disk so downstream
// clients (CLI, dashboard, third-party consumers, docs site) can be
// generated from it. The supervisor's Huma API owns every operation,
// so we fetch /openapi.json directly from a supervisor constructed
// against an empty resolver — no merge step, no per-city spec to
// combine, one authoritative source of truth.
//
// Default run (no flags) writes the spec to both canonical locations
// relative to the current working directory (typically the repo
// root when invoked via `go run ./cmd/genspec`):
//
//	internal/api/openapi.json   — drift-check source of truth
//	docs/schema/openapi.json    — committed docs copy
//	docs/schema/openapi.txt     — Mint-served download mirror
//
// Pass -out <path> to write a single file instead, or -stdout to
// emit to stdout (useful for ad-hoc inspection or legacy tooling).
//
// If the written internal/api/openapi.json drifts from what the
// running supervisor serves, TestOpenAPISpecInSync fails.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

func main() {
	var outFlag string
	var stdoutFlag bool
	flag.StringVar(&outFlag, "out", "", "Write the spec to this single path instead of the default two locations.")
	flag.BoolVar(&stdoutFlag, "stdout", false, "Write the spec to stdout instead of disk.")
	flag.Parse()

	sm := api.NewSupervisorMux(emptyResolver{}, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		fmt.Fprintf(os.Stderr, "GET /openapi.json returned %d: %s\n", rec.Code, rec.Body.String())
		os.Exit(1)
	}

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

	switch {
	case stdoutFlag:
		if _, err := os.Stdout.Write(out.Bytes()); err != nil {
			fmt.Fprintf(os.Stderr, "write stdout: %v\n", err)
			os.Exit(1)
		}
	case outFlag != "":
		writeSpec(outFlag, out.Bytes())
	default:
		writeSpec(filepath.Join("internal", "api", "openapi.json"), out.Bytes())
		writeSpec(filepath.Join("docs", "schema", "openapi.json"), out.Bytes())
		writeSpec(filepath.Join("docs", "schema", "openapi.txt"), out.Bytes())
	}
}

// writeSpec writes data to path, creating parent directories if needed.
func writeSpec(path string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir %s: %v\n", filepath.Dir(path), err)
		os.Exit(1)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", path, err)
		os.Exit(1)
	}
}

// emptyResolver implements api.CityResolver with no cities. Schema
// generation is reflection-based and never calls resolver methods.
type emptyResolver struct{}

func (emptyResolver) ListCities() []api.CityInfo      { return nil }
func (emptyResolver) CityState(name string) api.State { return nil }
