package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startingCityInfo is a city the supervisor lists mid-startup: known, not yet
// running, with the status the supervisor already tracks (ga-lkcyix).
var startingCityInfo = CityInfo{
	Name:            "gc-management",
	Path:            "/tmp/gc-management",
	Running:         false,
	Status:          "adopting_sessions",
	PhasesCompleted: []string{"loading_config", "starting_bead_store", "resolving_formulas"},
}

func newMuxListing(t *testing.T, cities ...CityInfo) *SupervisorMux {
	t.Helper()
	resolver := &fakeCityResolver{cities: map[string]*fakeState{}, listed: cities}
	return NewSupervisorMux(resolver, nil, false, "test", "", time.Now())
}

type problemResponse struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
}

func serveProblem(t *testing.T, sm *SupervisorMux, path, accept string) (*httptest.ResponseRecorder, problemResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)
	var p problemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode problem body %q: %v", rec.Body.String(), err)
	}
	return rec, p
}

// A city-scoped request against a city the supervisor knows is still starting
// answers 503 with Retry-After and a detail naming the startup status, on every
// path that used to answer the genuine-outage 404: Huma ops (bindCity), SSE
// streams (sseCityPrecheck) and the service proxy (serveCityRequest).
func TestCityScopedRequestToStartingCityReturns503(t *testing.T) {
	for _, tc := range []struct{ name, path, accept string }{
		{"huma op", "/v0/city/gc-management/agents", ""},
		{"sse precheck", "/v0/city/gc-management/events/stream", "text/event-stream"},
		{"service proxy", "/v0/city/gc-management/svc/review-intake/healthz", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := newMuxListing(t, startingCityInfo)
			rec, p := serveProblem(t, sm, tc.path, tc.accept)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
			}
			if secs, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || secs <= 0 {
				t.Errorf("Retry-After = %q, want a positive integer number of seconds", rec.Header().Get("Retry-After"))
			}
			if p.Status != http.StatusServiceUnavailable {
				t.Errorf("problem status = %d, want 503", p.Status)
			}
			if !IsCityStartingDetail(p.Detail) {
				t.Errorf("detail %q is not recognized by IsCityStartingDetail", p.Detail)
			}
			if IsCityNotFoundOrNotRunningDetail(p.Detail) {
				t.Errorf("starting detail %q must not match the genuine-404 predicate", p.Detail)
			}
			for _, want := range []string{"gc-management", "adopting_sessions"} {
				if !strings.Contains(p.Detail, want) {
					t.Errorf("detail %q does not name %q", p.Detail, want)
				}
			}
			if !strings.Contains(strings.ToLower(p.Detail), "retry") {
				t.Errorf("detail %q should tell a human to wait and retry", p.Detail)
			}
		})
	}
}

// A city that is unknown, registered but stopped, or failed to initialize is
// not "starting": today's 404 body stays exactly as callers match on it, with
// no Retry-After.
func TestCityScopedRequestToNonStartingCityKeeps404(t *testing.T) {
	for _, tc := range []struct {
		name string
		city string
		info []CityInfo
	}{
		{"unknown", "ghost", nil},
		{"registered but stopped", "parked", []CityInfo{{Name: "parked", Path: "/tmp/parked"}}},
		{"init failed", "broken", []CityInfo{{Name: "broken", Path: "/tmp/broken", Status: "init_failed", Error: "boom"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := newMuxListing(t, tc.info...)
			rec, p := serveProblem(t, sm, "/v0/city/"+tc.city+"/agents", "")

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", rec.Code, rec.Body.String())
			}
			if want := CityNotFoundOrNotRunningDetail(tc.city); p.Detail != want {
				t.Errorf("detail = %q, want %q", p.Detail, want)
			}
			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Errorf("Retry-After = %q, want none on a genuine 404", got)
			}
		})
	}
}

// The two details are told apart by separate predicates: callers match the
// genuine-404 prefix and must not start treating a starting city as gone.
func TestCityDetailPredicatesDoNotOverlap(t *testing.T) {
	notFound := CityNotFoundOrNotRunningDetail("gc-management")
	starting := CityStartingDetail("gc-management", "adopting_sessions")

	if !IsCityNotFoundOrNotRunningDetail(notFound) || IsCityStartingDetail(notFound) {
		t.Errorf("genuine-404 detail %q misclassified", notFound)
	}
	if !IsCityStartingDetail(starting) || IsCityNotFoundOrNotRunningDetail(starting) {
		t.Errorf("starting detail %q misclassified", starting)
	}
}

// `gc session list` against a starting city must surface the wait message, not
// fall back to a local read of a store that is still coming up and not be
// misread as "not found".
func TestClientStartingCityIsWaitMessageNotFallback(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"title":  "Service Unavailable",
			"status": http.StatusServiceUnavailable,
			"detail": CityStartingDetail("gc-management", "adopting_sessions"),
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "gc-management")
	_, err := c.ListSessions("", "", false)
	if err == nil {
		t.Fatal("expected an error for a starting city, got nil")
	}
	if ShouldFallbackForRead(c, err) {
		t.Errorf("ShouldFallbackForRead = true for a starting city: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"gc-management", "adopting_sessions"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %q", msg, want)
		}
	}
	if !strings.Contains(strings.ToLower(msg), "retry") {
		t.Errorf("error %q should read as wait-and-retry", msg)
	}
	if strings.Contains(msg, "not_found") || strings.Contains(msg, "not found") {
		t.Errorf("error %q must not read as not-found", msg)
	}
}
