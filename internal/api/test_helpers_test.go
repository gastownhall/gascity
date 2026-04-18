package api

import (
	"net/http"
	"testing"
	"time"
)

// newTestCityHandler returns an http.Handler that wraps a single State
// in a SupervisorMux, using the State's CityName() as the registered
// city name so test assertions against that name keep working.
// Tests that want to drive a per-city-scoped endpoint do:
//
//	h := newTestCityHandler(t, fs)
//	req := httptest.NewRequest("GET", cityURL(fs, "/config"), nil)
//	h.ServeHTTP(w, req)
//
// Accepts any api.State so tests can pass either *fakeState or
// *fakeMutatorState. For scenarios that need multiple cities or
// non-default naming, use newTestSupervisorMux directly.
func newTestCityHandler(t *testing.T, state State) http.Handler {
	t.Helper()
	return wrapTestSupervisorMiddleware(NewSupervisorMux(&stateCityResolver{state: state}, false, "test", time.Now()))
}

// newTestCityHandlerReadOnly is newTestCityHandler but with readOnly=true.
func newTestCityHandlerReadOnly(t *testing.T, state State) http.Handler {
	t.Helper()
	return wrapTestSupervisorMiddleware(NewSupervisorMux(&stateCityResolver{state: state}, true, "test", time.Now()))
}

// wrapTestSupervisorMiddleware applies the same middleware the supervisor's
// production Handler() does — minus the pprof listener goroutine, which
// would try to bind localhost:6060 on every test.
func wrapTestSupervisorMiddleware(sm *SupervisorMux) http.Handler {
	inner := http.HandlerFunc(sm.ServeHTTP)
	return withLogging(withRecovery(withRequestID(withCORS(inner))))
}

// stateCityResolver is a CityResolver backed by a single State. Used by
// newTestCityHandler / newTestCityHandlerReadOnly to adapt any State
// (fakeState, fakeMutatorState, etc.) into the CityResolver interface.
type stateCityResolver struct {
	state State
}

func (r *stateCityResolver) ListCities() []CityInfo {
	return []CityInfo{{
		Name:    r.state.CityName(),
		Path:    r.state.CityPath(),
		Running: true,
	}}
}

func (r *stateCityResolver) CityState(name string) State {
	if name == r.state.CityName() {
		return r.state
	}
	return nil
}

// cityURL prefixes path with "/v0/city/<state.CityName()>/" so tests
// can write URLs relative to a city's Huma API surface. Leading slash
// on path is required.
func cityURL(state State, path string) string {
	return "/v0/city/" + state.CityName() + path
}

// newTestCityHandlerWith wraps a caller-provided *Server in a single-city
// supervisor so tests that inject per-Server test fields (LookPathFunc,
// SlingRunnerFunc, sessionLogSearchPaths) can exercise their handler
// via HTTP. Pre-seeds the supervisor's per-city cache with the caller's
// Server so handler dispatch runs against that exact instance.
func newTestCityHandlerWith(t *testing.T, state State, srv *Server) http.Handler {
	t.Helper()
	sm := NewSupervisorMux(&stateCityResolver{state: state}, false, "test", time.Now())
	sm.cacheMu.Lock()
	sm.cache[state.CityName()] = cachedCityServer{state: state, srv: srv}
	sm.cacheMu.Unlock()
	return wrapTestSupervisorMiddleware(sm)
}
