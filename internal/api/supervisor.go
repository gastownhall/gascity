package api

import (
	"context"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof handlers on DefaultServeMux
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/events"
)

// CityInfo describes a managed city for the /v0/cities endpoint.
type CityInfo struct {
	Name            string   `json:"name"`
	Path            string   `json:"path"`
	Running         bool     `json:"running"`
	Status          string   `json:"status,omitempty"`
	Error           string   `json:"error,omitempty"`
	PhasesCompleted []string `json:"phases_completed,omitempty"`
}

// CityResolver provides city lookup for the supervisor API router.
type CityResolver interface {
	// ListCities returns all managed cities with status info.
	ListCities() []CityInfo
	// CityState returns the State for a named city, or nil if not found/not running.
	CityState(name string) State
}

// cachedCityServer pairs a State with its pre-built Server for caching.
type cachedCityServer struct {
	state State
	srv   *Server
}

// SupervisorMux routes API requests to per-city handlers with
// city-namespaced URL paths. It handles:
//   - GET /v0/cities — list managed cities
//   - GET /v0/city/{name} — city detail (status)
//   - /v0/city/{name}/... — route to a specific city's API
//   - /v0/city/{name}/svc/... — route to a specific city's service mount
//   - GET /health — supervisor health
//   - /v0/... (bare) — backward compat, routes to first running city
//   - /svc/... (bare) — route to the sole running city's service mount
//
// Supervisor-scope endpoints (cities, health, events, readiness,
// provider-readiness, POST /v0/city, global events stream) are registered
// as Huma operations on humaAPI and dispatched via humaMux. City-routing
// and backward-compat paths continue to be handled by ServeHTTP directly.
type SupervisorMux struct {
	resolver  CityResolver
	readOnly  bool
	version   string
	startedAt time.Time
	server    *http.Server

	// Supervisor-scope Huma API. Holds the 7 supervisor-only operations
	// (Phase 3 Fix 3b); per-city operations remain on per-city Servers.
	humaMux *http.ServeMux
	humaAPI huma.API

	// Per-city Server cache. Keyed by city name. Invalidated when
	// the State pointer changes (city restarted → new controllerState).
	cacheMu sync.RWMutex
	cache   map[string]cachedCityServer
}

// NewSupervisorMux creates a SupervisorMux that routes requests to cities
// resolved by the given CityResolver.
func NewSupervisorMux(resolver CityResolver, readOnly bool, version string, startedAt time.Time) *SupervisorMux {
	humaMux := http.NewServeMux()
	sm := &SupervisorMux{
		resolver:  resolver,
		readOnly:  readOnly,
		version:   version,
		startedAt: startedAt,
		humaMux:   humaMux,
		humaAPI:   newSupervisorHumaAPI(humaMux, readOnly),
		cache:     make(map[string]cachedCityServer),
	}
	sm.registerSupervisorRoutes()
	sm.server = &http.Server{Handler: sm.Handler()}
	return sm
}

// isSupervisorHumaPath returns true when (method, path) is a supervisor-
// scope endpoint handled by the supervisor Huma API. Called by ServeHTTP
// to route supervisor-scope traffic through Huma before falling back to
// raw city-routing and backward-compat paths.
func isSupervisorHumaPath(method, path string) bool {
	// Huma's auto-registered spec/docs paths — supervisor-scope spec.
	if method == http.MethodGet {
		switch path {
		case "/openapi.json", "/openapi.yaml", "/openapi-3.0.json", "/openapi-3.0.yaml", "/docs":
			return true
		}
	}
	if method != http.MethodGet && !(method == http.MethodPost && path == "/v0/city") {
		return false
	}
	switch path {
	case "/v0/cities",
		"/health",
		"/v0/readiness",
		"/v0/provider-readiness",
		"/v0/city",
		"/v0/events",
		"/v0/events/stream":
		return true
	}
	return false
}

// Handler returns an http.Handler with the standard middleware chain applied.
//
// Middleware layering (Phase 3 Fix 3b + 3d):
//   - Outermost (mux-level): withLogging, withRecovery, withCORS — these
//     stay at the mux level so /svc/* and any raw routes get panic coverage.
//   - CSRF and read-only for supervisor-scope Huma ops are enforced via
//     api.UseMiddleware on humaAPI (see newSupervisorHumaAPI).
//   - City-scoped forwarded routes inherit CSRF/read-only from the per-city
//     Server's own middleware stack.
//   - /svc/* paths bypass CSRF/read-only entirely (workspace services apply
//     their own publication rules).
func (sm *SupervisorMux) Handler() http.Handler {
	root := http.HandlerFunc(sm.ServeHTTP)
	// pprof: expose on a separate port for profiling
	go func() {
		_ = http.ListenAndServe("localhost:6060", nil) // default mux has pprof handlers
	}()
	return withLogging(withRecovery(withCORS(root)))
}

// Serve accepts connections on lis. Blocks until stopped.
func (sm *SupervisorMux) Serve(lis net.Listener) error {
	return sm.server.Serve(lis)
}

// Shutdown gracefully shuts down the server.
func (sm *SupervisorMux) Shutdown(ctx context.Context) error {
	return sm.server.Shutdown(ctx)
}

// ServeHTTP dispatches requests to the appropriate city or supervisor-level handler.
func (sm *SupervisorMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Supervisor-scope Huma operations: /v0/cities, /health, /v0/readiness,
	// /v0/provider-readiness, POST /v0/city, /v0/events, /v0/events/stream.
	// Huma enforces CSRF/read-only via api.UseMiddleware on humaAPI.
	if isSupervisorHumaPath(r.Method, path) {
		sm.humaMux.ServeHTTP(w, r)
		return
	}

	// City-namespaced: /v0/city/{name} or /v0/city/{name}/...
	if strings.HasPrefix(path, "/v0/city/") {
		rest := strings.TrimPrefix(path, "/v0/city/")
		idx := strings.IndexByte(rest, '/')
		var cityName, suffix string
		if idx < 0 {
			cityName = rest
			suffix = ""
		} else {
			cityName = rest[:idx]
			suffix = rest[idx:] // e.g. "/agents"
		}
		if cityName == "" {
			writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest), "bad_request: city name required in URL")
			return
		}
		var targetPath string
		switch {
		case suffix == "":
			targetPath = "/v0/status"
		case strings.HasPrefix(suffix, "/svc/"):
			targetPath = suffix
		default:
			targetPath = "/v0" + suffix
		}
		sm.serveCityRequest(w, r, cityName, targetPath)
		return
	}

	// Bare /v0/... and /svc/... — backward compat, route to the sole running
	// city. When multiple cities are running, require explicit city scope.
	if strings.HasPrefix(path, "/v0/") || path == "/v0" || strings.HasPrefix(path, "/svc/") {
		cities := sm.resolver.ListCities()
		var running []CityInfo
		for _, c := range cities {
			if c.Running {
				running = append(running, c)
			}
		}
		switch len(running) {
		case 0:
			writeProblemDetails(w, http.StatusServiceUnavailable, problemDetailsTitle(http.StatusServiceUnavailable), "no_cities: no cities running")
		case 1:
			sm.serveCityRequest(w, r, running[0].Name, path)
		default:
			writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest),
				"city_required: multiple cities running; use /v0/city/{name}/... to specify which city")
		}
		return
	}

	http.NotFound(w, r)
}

// serveCityRequest resolves a city's State and dispatches to a per-city Server.
func (sm *SupervisorMux) serveCityRequest(w http.ResponseWriter, r *http.Request, cityName, path string) {
	t0 := time.Now()
	state := sm.resolver.CityState(cityName)
	if state == nil {
		sm.cacheMu.Lock()
		delete(sm.cache, cityName)
		sm.cacheMu.Unlock()
		writeProblemDetails(w, http.StatusNotFound, problemDetailsTitle(http.StatusNotFound), "not_found: city not found or not running: "+cityName)
		return
	}
	t1 := time.Now()

	srv := sm.getCityServer(cityName, state)
	t2 := time.Now()

	r2 := r.Clone(r.Context())
	r2.URL.Path = path
	r2.URL.RawPath = ""
	srv.mux.ServeHTTP(w, r2)
	t3 := time.Now()

	total := t3.Sub(t0)
	if total > 500*time.Millisecond {
		log.Printf("SLOW serveCityRequest %s: resolve=%s getServer=%s handler=%s total=%s",
			path, t1.Sub(t0), t2.Sub(t1), t3.Sub(t2), total)
	}
}

// getCityServer returns a cached per-city Server, creating one if the
// cache is empty or the State pointer changed (city was restarted).
func (sm *SupervisorMux) getCityServer(name string, state State) *Server {
	sm.cacheMu.RLock()
	if cached, ok := sm.cache[name]; ok && cached.state == state {
		sm.cacheMu.RUnlock()
		return cached.srv
	}
	sm.cacheMu.RUnlock()

	srv := New(state)
	if sm.readOnly {
		srv = NewReadOnly(state)
	}

	sm.cacheMu.Lock()
	sm.cache[name] = cachedCityServer{state: state, srv: srv}
	sm.cacheMu.Unlock()

	return srv
}

func supervisorServicePath(path string) bool {
	if strings.HasPrefix(path, "/svc/") {
		return true
	}
	if !strings.HasPrefix(path, "/v0/city/") {
		return false
	}
	rest := strings.TrimPrefix(path, "/v0/city/")
	idx := strings.IndexByte(rest, '/')
	if idx < 0 {
		return false
	}
	return strings.HasPrefix(rest[idx:], "/svc/")
}

// buildMultiplexer creates a Multiplexer from all running cities'
// event providers.
func (sm *SupervisorMux) buildMultiplexer() *events.Multiplexer {
	mux := events.NewMultiplexer()
	cities := sm.resolver.ListCities()
	for _, c := range cities {
		if !c.Running {
			continue
		}
		state := sm.resolver.CityState(c.Name)
		if state == nil {
			continue
		}
		ep := state.EventProvider()
		if ep == nil {
			continue
		}
		mux.Add(c.Name, ep)
	}
	return mux
}

// allStartupPhases returns the ordered list of all startup phases.
func allStartupPhases() []string {
	return []string{
		"loading_config",
		"starting_bead_store",
		"resolving_formulas",
		"adopting_sessions",
		"starting_agents",
	}
}
