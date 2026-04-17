package api

import (
	"net/http"
	"testing"
	"time"
)

// testCityName is the default city name used by newTestCityHandler.
// All per-city-scoped URLs in tests look like
// "/v0/city/<testCityName>/foo". Callers that need a different name
// should construct a SupervisorMux directly via newTestSupervisorMux.
const testCityName = "test"

// newTestCityHandler returns an http.Handler that wraps a single fake
// state in a SupervisorMux under the name testCityName. Tests that
// want to drive a per-city-scoped endpoint do:
//
//	h := newTestCityHandler(t, fs)
//	req := httptest.NewRequest("GET", cityURL("/v0/config"), nil)
//	h.ServeHTTP(w, req)
//
// For scenarios that need multiple cities or non-default naming, use
// newTestSupervisorMux directly.
func newTestCityHandler(t *testing.T, fs *fakeState) http.Handler {
	t.Helper()
	fs.cityName = testCityName
	resolver := &fakeCityResolver{cities: map[string]*fakeState{testCityName: fs}}
	return NewSupervisorMux(resolver, false, "test", time.Now())
}

// newTestCityHandlerReadOnly is newTestCityHandler but with readOnly=true.
func newTestCityHandlerReadOnly(t *testing.T, fs *fakeState) http.Handler {
	t.Helper()
	fs.cityName = testCityName
	resolver := &fakeCityResolver{cities: map[string]*fakeState{testCityName: fs}}
	return NewSupervisorMux(resolver, true, "test", time.Now())
}

// cityURL prefixes path with "/v0/city/test" so tests can write URLs
// relative to a city's Huma API surface. Leading slash on path is
// required.
func cityURL(path string) string {
	return "/v0/city/" + testCityName + path
}
