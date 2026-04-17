// Package specmerge builds the authoritative OpenAPI spec for the Gas
// City HTTP control plane by merging the per-city Server's spec with
// the SupervisorMux's spec.
//
// Phase 3 Fix 3b split the control plane across two Huma APIs: per-city
// operations live on api.Server.humaAPI, supervisor-scope operations
// (e.g. /v0/cities, POST /v0/city, /health, /v0/events) live on
// SupervisorMux.humaAPI. Neither API alone describes the full surface a
// consumer sees. This package merges them so there is a single
// authoritative spec committed to disk and consumed by the generator.
//
// Usage:
//
//	spec, err := specmerge.Merged("/openapi.json")       // 3.1
//	spec, err := specmerge.Merged("/openapi-3.0.json")   // 3.0 downgrade
//
// The merged spec is mutated in place on the per-city copy. Duplicate
// paths keep the per-city variant; duplicate component schemas must be
// byte-identical or the merge errors out.
package specmerge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// Merged returns the merged per-city + supervisor OpenAPI spec at the
// given Huma-served path (typically "/openapi.json" for 3.1 or
// "/openapi-3.0.json" for the 3.0 downgrade consumed by oapi-codegen).
func Merged(path string) (map[string]any, error) {
	citySpec, err := fetchSpec(api.New(stubState{}), path)
	if err != nil {
		return nil, fmt.Errorf("per-city spec: %w", err)
	}
	supSpec, err := fetchSpec(api.NewSupervisorMux(emptyResolver{}, false, "", time.Time{}), path)
	if err != nil {
		return nil, fmt.Errorf("supervisor spec: %w", err)
	}
	if err := mergeSpecs(citySpec, supSpec); err != nil {
		return nil, fmt.Errorf("merge: %w", err)
	}
	return citySpec, nil
}

// fetchSpec issues GET against h and returns the parsed JSON body.
func fetchSpec(h http.Handler, path string) (map[string]any, error) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", path, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// mergeSpecs folds src paths and component schemas into dst (mutated in
// place). Duplicate paths keep the dst variant (see comment below).
// Duplicate component schemas must be byte-identical, otherwise this
// returns an error so schema drift between the two APIs surfaces loudly.
func mergeSpecs(dst, src map[string]any) error {
	srcPaths, _ := src["paths"].(map[string]any)
	dstPaths, _ := dst["paths"].(map[string]any)
	if dstPaths == nil {
		dstPaths = map[string]any{}
		dst["paths"] = dstPaths
	}
	for k, v := range srcPaths {
		if _, exists := dstPaths[k]; exists {
			// Duplicate path. Only known case is /v0/events/stream
			// (per-city emits city-scope events; supervisor emits
			// cross-city tagged events). Keep the per-city variant —
			// SSE clients do not go through the generated typed client
			// anyway. If a future merge surfaces a real REST conflict,
			// disambiguate via per-call adapter.
			a, _ := json.Marshal(dstPaths[k])
			b, _ := json.Marshal(v)
			if string(a) != string(b) {
				// No-op: first-wins is the documented policy.
				_ = a
				_ = b
			}
			continue
		}
		dstPaths[k] = v
	}

	srcComp, _ := src["components"].(map[string]any)
	if srcComp == nil {
		return nil
	}
	dstComp, _ := dst["components"].(map[string]any)
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

// stubState is a minimal api.State that returns zero values. Huma's
// schema generation is reflection-based and never calls State methods,
// so zero-value returns are safe even though some would be nonsensical
// at runtime.
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

// emptyResolver is an api.CityResolver with no cities. Schema
// generation never calls resolver methods, so zero-value returns are
// safe.
type emptyResolver struct{}

func (emptyResolver) ListCities() []api.CityInfo      { return nil }
func (emptyResolver) CityState(name string) api.State { return nil }
