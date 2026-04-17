// Command gen-client generates the typed Go API client from the live
// OpenAPI spec.
//
// Pipeline:
//  1. Fetch the 3.0-downgrade spec directly from a SupervisorMux built
//     against an empty resolver. Huma v2 emits the downgrade
//     automatically; oapi-codegen v2.6.0 consumes it cleanly where it
//     chokes on 3.1. The supervisor owns every operation, so one fetch
//     yields the entire API surface — no merge step.
//  2. Preprocess:
//       a. Path params `{name...}` (Huma's rest-of-path syntax) are
//          renamed to `{name}` to match the declared parameter.
//       b. Component schemas matching `^(Get|Post|Put|Patch|Delete|
//          Head|Options)-.*Response$` (Huma auto-generates these for
//          anonymous response bodies) have their `Response` suffix
//          replaced with `Body`, avoiding collision with oapi-codegen's
//          per-operation `<OpId>Response` wrapper type.
//  3. Invoke oapi-codegen (must be on PATH).
//  4. Write the generated client to internal/api/genclient/client_gen.go.
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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
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
	sm := api.NewSupervisorMux(emptyResolver{}, false, "", time.Time{})
	req := httptest.NewRequest(http.MethodGet, "/openapi-3.0.json", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET /openapi-3.0.json returned %d: %s", rec.Code, rec.Body.String())
	}
	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		return fmt.Errorf("parse spec: %w", err)
	}

	// Step 2a: normalize path params (`{name...}` → `{name}`).
	if paths, ok := spec["paths"].(map[string]any); ok {
		renamed := make(map[string]any, len(paths))
		for k, v := range paths {
			renamed[pathParamRE.ReplaceAllString(k, "{$1}")] = v
		}
		spec["paths"] = renamed
	}

	// Step 2b: rename `^<Verb>-.*Response$` component schemas to `*Body`.
	renameMap := map[string]string{}
	if components, ok := spec["components"].(map[string]any); ok {
		if schemas, ok := components["schemas"].(map[string]any); ok {
			for name := range schemas {
				if responseBodyRE.MatchString(name) {
					renameMap[name] = name[:len(name)-len("Response")] + "Body"
				}
			}
			for old, new := range renameMap {
				schemas[new] = schemas[old]
				delete(schemas, old)
			}
		}
	}
	if len(renameMap) > 0 {
		rewriteRefs(spec, renameMap)
	}

	// Step 3: write the transformed spec to a temp file.
	tmp, err := os.CreateTemp("", "gc-openapi-3.0-*.json")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(spec); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp spec: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp spec: %w", err)
	}

	// Step 4: invoke oapi-codegen. Output goes to stdout — the caller
	// redirects it to internal/api/genclient/client_gen.go.
	cmd := exec.Command("oapi-codegen", "-generate", "types,client", "-package", "genclient", tmp.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("oapi-codegen: %w", err)
	}
	return nil
}

var (
	pathParamRE    = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\.\.\.\}`)
	responseBodyRE = regexp.MustCompile(`^(?:Get|Post|Put|Patch|Delete|Head|Options)-.*Response$`)
)

// emptyResolver implements api.CityResolver with no cities. Schema
// generation is reflection-based and never calls resolver methods.
type emptyResolver struct{}

func (emptyResolver) ListCities() []api.CityInfo      { return nil }
func (emptyResolver) CityState(name string) api.State { return nil }

// rewriteRefs walks spec and rewrites any "$ref": "#/components/schemas/<old>"
// values to the new name.
func rewriteRefs(node any, rename map[string]string) {
	switch v := node.(type) {
	case map[string]any:
		for k, vv := range v {
			if k == "$ref" {
				if s, ok := vv.(string); ok {
					const prefix = "#/components/schemas/"
					if len(s) > len(prefix) && s[:len(prefix)] == prefix {
						tail := s[len(prefix):]
						if replacement, ok := rename[tail]; ok {
							v[k] = prefix + replacement
						}
					}
				}
			} else {
				rewriteRefs(vv, rename)
			}
		}
	case []any:
		for _, item := range v {
			rewriteRefs(item, rename)
		}
	}
}
