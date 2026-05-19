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
//	docs/schema/events.json     — gc events JSONL line schema
//	docs/schema/events.txt      — Mint-served download mirror
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

	// Spec generation does not exercise city creation; nil Initializer
	// leaves POST /v0/city returning 501 in the live spec, which is
	// not observable at spec generation time.
	sm := api.NewSupervisorMux(emptyResolver{}, nil, false, "", time.Time{})
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

		events, err := eventsSpec()
		if err != nil {
			fmt.Fprintf(os.Stderr, "generate events schema: %v\n", err)
			os.Exit(1)
		}
		writeSpec(filepath.Join("docs", "schema", "events.json"), events)
		writeSpec(filepath.Join("docs", "schema", "events.txt"), events)
	}
}

func eventsSpec() ([]byte, error) {
	schema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     "https://docs.gascityhall.com/schema/events.json",
		"title":   "gc events JSONL line schema",
		"description": "Validates one JSON object line emitted by `gc events`, `gc events --watch`, or `gc events --follow`. " +
			"The referenced DTO schemas live in the supervisor OpenAPI document; the API remains the source of truth. " +
			"`gc events --seq` emits a plain-text cursor and is documented in /reference/events.",
		"anyOf": []any{
			map[string]any{"$ref": "openapi.json#/components/schemas/TypedEventStreamEnvelope"},
			map[string]any{"$ref": "openapi.json#/components/schemas/TypedTaggedEventStreamEnvelope"},
			map[string]any{"$ref": "openapi.json#/components/schemas/EventStreamEnvelope"},
			map[string]any{"$ref": "openapi.json#/components/schemas/TaggedEventStreamEnvelope"},
		},
		"$defs": map[string]any{
			"cityListLine": map[string]any{
				"description": "A JSONL line from `gc events` when a city is in scope.",
				"$ref":        "openapi.json#/components/schemas/TypedEventStreamEnvelope",
			},
			"cityStreamLine": map[string]any{
				"description": "A JSONL line from `gc events --watch` or `gc events --follow` when a city is in scope.",
				"$ref":        "openapi.json#/components/schemas/EventStreamEnvelope",
			},
			"supervisorListLine": map[string]any{
				"description": "A JSONL line from `gc events` when no city is in scope.",
				"$ref":        "openapi.json#/components/schemas/TypedTaggedEventStreamEnvelope",
			},
			"supervisorStreamLine": map[string]any{
				"description": "A JSONL line from `gc events --watch` or `gc events --follow` when no city is in scope.",
				"$ref":        "openapi.json#/components/schemas/TaggedEventStreamEnvelope",
			},
		},
		"x-gc-events": map[string]any{
			"sourceOfTruth":        "openapi.json",
			"listMode":             []string{"TypedEventStreamEnvelope", "TypedTaggedEventStreamEnvelope"},
			"streamMode":           []string{"EventStreamEnvelope", "TaggedEventStreamEnvelope"},
			"heartbeatSuppression": "HeartbeatEvent SSE frames are consumed internally and are not written to stdout.",
			"cursorMode":           "`gc events --seq` is not JSONL; it writes the current city index or supervisor composite cursor as text.",
		},
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(schema); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
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

func (emptyResolver) ListCities() []api.CityInfo   { return nil }
func (emptyResolver) CityState(_ string) api.State { return nil }
