package api

import (
	"net/http"
	"testing"
	"time"
)

// newTestCityHandler returns an http.Handler that wraps a single fake
// state in a SupervisorMux, using the fake's existing CityName() as the
// registered city name so test assertions against that name keep working.
// Tests that want to drive a per-city-scoped endpoint do:
//
//	h := newTestCityHandler(t, fs)
//	req := httptest.NewRequest("GET", cityURL(fs, "/config"), nil)
//	h.ServeHTTP(w, req)
//
// For scenarios that need multiple cities or non-default naming, use
// newTestSupervisorMux directly.
func newTestCityHandler(t *testing.T, fs *fakeState) http.Handler {
	t.Helper()
	resolver := &fakeCityResolver{cities: map[string]*fakeState{fs.CityName(): fs}}
	return NewSupervisorMux(resolver, false, "test", time.Now())
}

// newTestCityHandlerReadOnly is newTestCityHandler but with readOnly=true.
func newTestCityHandlerReadOnly(t *testing.T, fs *fakeState) http.Handler {
	t.Helper()
	resolver := &fakeCityResolver{cities: map[string]*fakeState{fs.CityName(): fs}}
	return NewSupervisorMux(resolver, true, "test", time.Now())
}

// cityURL prefixes path with "/v0/city/<fs.CityName()>/" so tests can
// write URLs relative to a city's Huma API surface. Leading slash on
// path is required.
func cityURL(fs *fakeState, path string) string {
	return "/v0/city/" + fs.CityName() + path
}
