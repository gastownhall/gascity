package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCORSPreflightFromLocalhostDashboard locks the contract the
// dashboard SPA depends on: a preflight from a localhost origin
// includes the headers the SPA needs (X-GC-Request CSRF header and
// Last-Event-ID for SSE resume) and returns 204 No Content.
//
// Breaking any of these assertions silently disables the dashboard
// cross-origin, so this test is a tripwire.
func TestCORSPreflightFromLocalhostDashboard(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	cases := []struct {
		name   string
		origin string
		method string
		path   string
	}{
		{"GET preflight", "http://127.0.0.1:8080", http.MethodGet, "/v0/cities"},
		{"POST mutation preflight", "http://localhost:8080", http.MethodPost, "/v0/city/test-city/beads"},
		{"SSE preflight", "http://127.0.0.1:8080", http.MethodGet, "/v0/events/stream"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, tc.path, nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", tc.method)
			req.Header.Set("Access-Control-Request-Headers", "Content-Type,X-GC-Request,Last-Event-ID")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusNoContent {
				t.Fatalf("preflight status = %d, want 204; body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.origin {
				t.Errorf("Allow-Origin = %q, want %q", got, tc.origin)
			}
			allowedHeaders := rec.Header().Get("Access-Control-Allow-Headers")
			for _, want := range []string{"X-GC-Request", "Last-Event-ID", "Content-Type"} {
				if !strings.Contains(allowedHeaders, want) {
					t.Errorf("Allow-Headers %q missing %q", allowedHeaders, want)
				}
			}
			allowedMethods := rec.Header().Get("Access-Control-Allow-Methods")
			for _, want := range []string{"GET", "POST", "PATCH", "DELETE"} {
				if !strings.Contains(allowedMethods, want) {
					t.Errorf("Allow-Methods %q missing %q", allowedMethods, want)
				}
			}
		})
	}
}

// TestCORSAllowsExtraOrigins verifies that WithAllowedOrigins extends CORS
// acceptance to explicitly listed non-localhost origins.
func TestCORSAllowsExtraOrigins(t *testing.T) {
	state := newFakeState(t)
	sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", time.Now())
	sm.WithAllowedOrigins([]string{"http://192.168.1.10:8080"})
	h := wrapTestSupervisorMiddleware(sm)

	cases := []struct {
		origin string
		want   string
	}{
		{"http://192.168.1.10:8080", "http://192.168.1.10:8080"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"http://192.168.1.99:8080", ""},
	}
	for _, tc := range cases {
		t.Run(tc.origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/v0/cities", nil)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Access-Control-Request-Method", "GET")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.want {
				t.Errorf("origin %q: Allow-Origin = %q, want %q", tc.origin, got, tc.want)
			}
		})
	}
}

// TestCORSRejectsNonLocalhostOrigin makes sure the permissive
// localhost policy doesn't leak to public origins. A fake
// "localhost.evil.com" must not be accepted.
func TestCORSRejectsNonLocalhostOrigin(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	badOrigins := []string{
		"http://localhost.evil.com",
		"http://127.0.0.1.evil.com",
		"http://example.com",
		"https://attacker.com:8080",
	}
	for _, origin := range badOrigins {
		t.Run(origin, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodOptions, "/v0/cities", nil)
			req.Header.Set("Origin", origin)
			req.Header.Set("Access-Control-Request-Method", "GET")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			// Preflight short-circuits with 204 regardless, but the
			// browser checks Allow-Origin — and for a non-localhost
			// origin it must be absent.
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("origin %q got Allow-Origin %q, want empty", origin, got)
			}
		})
	}
}
