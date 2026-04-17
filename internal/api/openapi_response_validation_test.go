package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
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

	doc, err := libopenapi.NewDocument(specBytes)
	if err != nil {
		t.Fatalf("build document: %v", err)
	}
	v, errs := validator.NewValidator(doc)
	if len(errs) > 0 {
		t.Fatalf("construct validator: %v", errs)
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

		// Per-city scope.
		{"city detail", "/v0/city/{cityName}"},
		{"agent list", "/v0/city/{cityName}/agents"},
		{"bead list", "/v0/city/{cityName}/beads"},
		{"session list", "/v0/city/{cityName}/sessions"},
		{"service list", "/v0/city/{cityName}/services"},
		{"formula list", "/v0/city/{cityName}/formulas"},
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
			defer resp.Body.Close()

			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			resp.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))

			// Validate whatever the op returned against the spec —
			// success OR declared error path. A spec-driven API has a
			// schema for every response the handler can emit; if the
			// handler emits something undeclared, validation fails and
			// that IS the signal the test is meant to catch.
			t.Logf("%s → status %d", tc.path, resp.StatusCode)

			ok, valErrs := v.ValidateHttpResponse(req, resp)
			if !ok {
				for _, ve := range valErrs {
					t.Errorf("%s: %s — %s", tc.path, ve.Message, ve.Reason)
					for _, se := range ve.SchemaValidationErrors {
						t.Errorf("  %s at %s", se.Reason, se.FieldPath)
					}
				}
				t.Fatalf("%s: response body does not match spec (see errors above). Body: %s", tc.path, string(bodyBytes))
			}
		})
	}
}
