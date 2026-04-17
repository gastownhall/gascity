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

// SupervisorMux owns the single Huma API for the entire control plane.
// Every typed operation — supervisor-scope and per-city — is registered
// on humaAPI:
//   - Supervisor-scope (registerSupervisorRoutes): GET /v0/cities,
//     GET /health, GET /v0/readiness, GET /v0/provider-readiness,
//     POST /v0/city, GET /v0/events, GET /v0/events/stream.
//   - Per-city (registerCityRoutes): every operation at
//     /v0/city/{cityName}/..., resolved at request time via bindCity.
//
// The only non-Huma registration on humaMux is serveCitySvcProxy at
// "/v0/city/{cityName}/svc/", which forwards workspace-service traffic
// to per-city Server.mux. Workspace services own their own HTTP
// contracts and are explicitly excluded from the typed control plane.
type SupervisorMux struct {
	resolver  CityResolver
	readOnly  bool
	version   string
	startedAt time.Time
	server    *http.Server

	// Single Huma API (Phase 3.5 — Topology 1). Owns every typed
	// operation: supervisor-scope (/v0/cities, /health, /v0/readiness,
	// /v0/provider-readiness, POST /v0/city, /v0/events,
	// /v0/events/stream) plus every per-city operation at
	// /v0/city/{cityName}/... registered via SupervisorMux.
	// registerCityRoutes. Per-city *Server instances exist only as
	// handler hosts for per-city state; they do not own a Huma API.
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
	sm.registerCityRoutes()
	// /svc/* workspace-service pass-through. This is the single remaining
	// non-Huma registration on the supervisor — untyped by design (the
	// proxy passes bodies through to external service processes, which
	// own their own HTTP contracts). Go 1.22+ mux: "/v0/city/{cityName}/svc/"
	// as a prefix pattern only matches that subtree; everything else is
	// a typed Huma operation registered at its real scoped path.
	humaMux.HandleFunc("/v0/city/{cityName}/svc/", sm.serveCitySvcProxy)
	sm.server = &http.Server{Handler: sm.Handler()}
	return sm
}

// serveCitySvcProxy forwards /v0/city/{cityName}/svc/... to the per-city
// Server's mux at /svc/... (where handleServiceProxy is registered).
// The /svc/* surface is explicitly excluded from the "spec drives
// everything" principle: it is a raw pass-through to external service
// processes that own their own HTTP contracts.
func (sm *SupervisorMux) serveCitySvcProxy(w http.ResponseWriter, r *http.Request) {
	cityName := r.PathValue("cityName")
	if cityName == "" {
		writeProblemDetails(w, http.StatusBadRequest, problemDetailsTitle(http.StatusBadRequest), "bad_request: city name required in URL")
		return
	}
	// Strip the /v0/city/<name> prefix; the remaining path is /svc/...
	// which per-city Server.mux handles via handleServiceProxy.
	svcPath := strings.TrimPrefix(r.URL.Path, "/v0/city/"+cityName)
	sm.serveCityRequest(w, r, cityName, svcPath)
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

// ServeHTTP delegates every request to humaMux. Every typed
// operation — supervisor-scope and city-scoped — is registered on the
// supervisor's single Huma API. The only non-Huma registration is
// serveCitySvcProxy at "/v0/city/{cityName}/svc/" for the
// workspace-service pass-through; Go 1.22+ mux specificity routes
// /v0/city/{cityName}/<typed-op> requests to the matching Huma
// operation rather than the prefix handler.
func (sm *SupervisorMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sm.humaMux.ServeHTTP(w, r)
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
