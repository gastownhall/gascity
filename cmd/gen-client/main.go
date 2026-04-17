// Command gen-client generates the typed Go API client from the live
// OpenAPI spec. Phase 3 Fix 3a.
//
// Pipeline:
//  1. Instantiate a stub api.Server, hit /openapi-3.0.json to get the
//     OpenAPI 3.0 downgrade of the live 3.1 spec. Huma v2 emits the
//     downgrade automatically; oapi-codegen v2.6.0 consumes it cleanly
//     where it chokes on 3.1.
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
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// Step 1a: fetch the 3.0-downgraded per-city spec from a stub server.
	citySpec, err := fetchSpec(api.New(stubState{}))
	if err != nil {
		return fmt.Errorf("per-city spec: %w", err)
	}

	// Step 1b: fetch the supervisor spec by instantiating SupervisorMux.
	// The supervisor's Huma API lives on its own mux (sm.humaMux); routing
	// through SupervisorMux.ServeHTTP recognizes /openapi-3.0.json as a
	// supervisor-scope path.
	supSpec, err := fetchSpec(api.NewSupervisorMux(emptyResolver{}, false, "", time.Time{}))
	if err != nil {
		return fmt.Errorf("supervisor spec: %w", err)
	}

	// Step 1c: merge supervisor paths and component schemas into the
	// per-city spec. Component name conflicts are extremely unlikely
	// because supervisor-only types are uniquely named (CityInfo,
	// SupervisorHealthOutput, etc.); when they DO collide on shared
	// schemas (Error, ListBodyTaggedEvent, etc.), the supervisor copy
	// is dropped because per-city already has an identical definition.
	if err := mergeSpecs(citySpec, supSpec); err != nil {
		return fmt.Errorf("merge specs: %w", err)
	}
	spec := citySpec

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

// fetchSpec issues GET /openapi-3.0.json against an http.Handler and
// returns the parsed JSON map.
func fetchSpec(h http.Handler) (map[string]any, error) {
	req := httptest.NewRequest(http.MethodGet, "/openapi-3.0.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("GET /openapi-3.0.json returned %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return out, nil
}

// mergeSpecs folds supervisor paths and unique component schemas into the
// per-city spec (mutated in place). Component name conflicts that aren't
// byte-identical produce an error; identical duplicates are silently
// dropped.
func mergeSpecs(dst, src map[string]any) error {
	srcPaths, _ := src["paths"].(map[string]any)
	dstPaths, _ := dst["paths"].(map[string]any)
	if dstPaths == nil {
		dstPaths = map[string]any{}
		dst["paths"] = dstPaths
	}
	for k, v := range srcPaths {
		if _, exists := dstPaths[k]; exists {
			// Duplicate paths exist — keep the per-city variant. The
			// only known conflict is /v0/events/stream (per-city emits
			// city-scope events; supervisor emits cross-city tagged
			// events) and SSE clients don't go through the generated
			// typed client anyway. If a future merge surfaces a real
			// REST conflict, add per-call disambiguation in the adapter.
			a, _ := json.Marshal(dstPaths[k])
			b, _ := json.Marshal(v)
			if string(a) != string(b) {
				fmt.Fprintf(os.Stderr, "gen-client: path %q exists in both specs with differing definitions; keeping per-city variant\n", k)
			}
			continue
		}
		dstPaths[k] = v
	}

	srcComp, _ := src["components"].(map[string]any)
	dstComp, _ := dst["components"].(map[string]any)
	if srcComp == nil {
		return nil
	}
	if dstComp == nil {
		dstComp = map[string]any{}
		dst["components"] = dstComp
	}
	srcSchemas, _ := srcComp["schemas"].(map[string]any)
	dstSchemas, _ := dstComp["schemas"].(map[string]any)
	if dstSchemas == nil {
		dstSchemas = map[string]any{}
		dstComp["schemas"] = dstSchemas
	}
	for k, v := range srcSchemas {
		if existing, exists := dstSchemas[k]; exists {
			a, _ := json.Marshal(existing)
			b, _ := json.Marshal(v)
			if string(a) != string(b) {
				return fmt.Errorf("schema %q exists in both specs with differing definitions", k)
			}
			continue
		}
		dstSchemas[k] = v
	}
	return nil
}

// emptyResolver implements api.CityResolver with no cities. Used by
// gen-client to instantiate a SupervisorMux for spec emission only —
// schema generation never calls resolver methods.
type emptyResolver struct{}

func (emptyResolver) ListCities() []api.CityInfo         { return nil }
func (emptyResolver) CityState(name string) api.State    { return nil }

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

// stubState mirrors cmd/genspec's stubState. Huma's schema generation is
// reflection-based and never calls State methods, so zero-value returns
// are safe.
type stubState struct{}

func (stubState) Config() *config.City                     { return &config.City{} }
func (stubState) SessionProvider() runtime.Provider        { return nil }
func (stubState) BeadStore(string) beads.Store             { return nil }
func (stubState) BeadStores() map[string]beads.Store       { return nil }
func (stubState) MailProvider(string) mail.Provider        { return nil }
func (stubState) MailProviders() map[string]mail.Provider  { return nil }
func (stubState) EventProvider() events.Provider           { return nil }
func (stubState) CityName() string                         { return "" }
func (stubState) CityPath() string                         { return "" }
func (stubState) Version() string                          { return "" }
func (stubState) StartedAt() time.Time                     { return time.Time{} }
func (stubState) IsQuarantined(string) bool                { return false }
func (stubState) ClearCrashHistory(string)                 {}
func (stubState) CityBeadStore() beads.Store               { return nil }
func (stubState) Orders() []orders.Order                   { return nil }
func (stubState) Poke()                                    {}
func (stubState) ServiceRegistry() workspacesvc.Registry   { return nil }
func (stubState) ExtMsgServices() *extmsg.Services         { return nil }
func (stubState) AdapterRegistry() *extmsg.AdapterRegistry { return nil }

var _ = bytes.NewReader // silence unused-import if tempfile path changes