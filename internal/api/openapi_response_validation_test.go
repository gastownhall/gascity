package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/apicontract"
)

// TestResponseBodiesMatchSpec drives a curated list of simple GET
// operations against a real supervisor-backed handler and validates
// each response against the operation's schema in the committed
// OpenAPI document. Huma does not validate responses at runtime, so
// drift between a handler and its declared response schema would
// only be caught by a consumer. This test catches it at build time.
//
// Scope (first pass): straightforward supervisor-scope GETs plus a
// handful of per-city GETs that work against the default fakeState.
// Operations that need specific seeded state (sessions with pending
// interactions, convoys mid-flight) are exercised by domain-specific
// tests already in the suite; this is the breadth check.
func TestResponseBodiesMatchSpec(t *testing.T) {
	specBytes, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}

	spec, err := apicontract.Load(specBytes)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}

	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	cases := []struct {
		name string
		path string // relative; {cityName} substituted below.
	}{
		// Supervisor scope.
		{"cities list", "/v0/cities"},
		{"readiness", "/v0/readiness"},
		{"provider readiness", "/v0/provider-readiness"},
		{"health", "/health"},

		// Per-city scope.
		{"city detail", "/v0/city/{cityName}"},
		{"city status", "/v0/city/{cityName}/status"},
		{"agent list", "/v0/city/{cityName}/agents"},
		{"bead list", "/v0/city/{cityName}/beads"},
		{"mail inbox", "/v0/city/{cityName}/mail"},
		{"convoy list", "/v0/city/{cityName}/convoys"},
		{"session list", "/v0/city/{cityName}/sessions"},
		{"service list", "/v0/city/{cityName}/services"},
		{"formula list", "/v0/city/{cityName}/formulas?scope_kind=city&scope_ref=test-city"},
		{"order list", "/v0/city/{cityName}/orders"},
		{"config", "/v0/city/{cityName}/config"},
		{"pack list", "/v0/city/{cityName}/packs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := ts.URL + strings.ReplaceAll(tc.path, "{cityName}", state.CityName())
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Accept", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			// Validate whatever the op returned against the spec —
			// success OR declared error path. A spec-driven API has a
			// schema for every response the handler can emit; if the
			// handler emits something undeclared, validation fails and
			// that IS the signal the test is meant to catch.
			t.Logf("%s → status %d", tc.path, resp.StatusCode)

			if err := spec.ValidateResponse(req, resp, bodyBytes); err != nil {
				t.Fatal(err)
			}
		})
	}
}
