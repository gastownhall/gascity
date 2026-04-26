package api

import (
	"context"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/cityinit"
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

// PendingRequestStore is an optional CityResolver extension that
// lets async handlers store correlation request IDs for later
// retrieval by the reconciler when emitting request.result events.
type PendingRequestStore interface {
	StorePendingRequestID(cityPath, requestID string)
	ConsumePendingRequestID(cityPath string) (string, bool)
}

// SupervisorEventSource is an optional CityResolver extension that
// provides a supervisor-level event recorder for city lifecycle events
// (create/unregister completion). These events belong on the supervisor
// scope because the city doesn't exist during create and goes away
// during unregister.
type SupervisorEventSource interface {
	SupervisorEventRecorder() events.Recorder
}

// TransientCityEventSource is an optional CityResolver extension
// that lets the supervisor-scope event multiplexer include event
// providers for cities that are registered but not yet (or no
// longer) in the Running set — newly scaffolded cities whose
// reconciler hasn't picked them up, cities currently running
// prepareCityForSupervisor, and cities whose init failed. Without
// this, /v0/events/stream subscribers can't observe city.created,
// city.ready, or city.init_failed for cities that aren't yet
// reporting Running=true through ListCities.
//
// Resolvers that implement this return one entry per transient
// city; the key is the city name, the value is an event provider
// backed by that city's .gc/events.jsonl file. The supervisor
// multiplexer adds these on top of the Running-city providers it
// already picks up via ListCities + CityState.
type TransientCityEventSource interface {
	TransientCityEventProviders() map[string]events.Provider
}

type cityInitializer interface {
	Scaffold(context.Context, cityinit.InitRequest) (*cityinit.InitResult, error)
	Unregister(context.Context, cityinit.UnregisterRequest) (*cityinit.UnregisterResult, error)
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
	resolver    CityResolver
	initializer cityInitializer
	readOnly    bool
	version     string
	startedAt   time.Time
	server      *http.Server

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
// resolved by the given CityResolver. The initializer is invoked by the
// POST /v0/city handler to scaffold new cities in-process; passing nil
// is allowed for tests that don't exercise city creation (the handler
// returns 501 Not Implemented in that case).
func NewSupervisorMux(resolver CityResolver, initializer cityInitializer, readOnly bool, version string, startedAt time.Time) *SupervisorMux {
	humaMux := http.NewServeMux()
	sm := &SupervisorMux{
		resolver:    resolver,
		initializer: initializer,
		readOnly:    readOnly,
		version:     version,
		startedAt:   startedAt,
		humaMux:     humaMux,
		humaAPI:     newSupervisorHumaAPI(humaMux, readOnly),
		cache:       make(map[string]cachedCityServer),
	}
	sm.registerSupervisorRoutes()
	sm.registerCityRoutes()
	documentProblemTypes(sm.humaAPI.OpenAPI())
	// Declare framework-level response headers (X-GC-Request-Id) via
	// components.headers + $ref on every operation. Middleware writes
	// the header at runtime; the spec describes the contract. Must run
	// after all routes are registered.
	registerFrameworkHeaders(sm.humaAPI)
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
		problemCityNameRequired.writeTo(w)
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
	return withLogging(withRecovery(withRequestID(withCORS(root))))
}

// StartPprof starts a pprof HTTP server on 127.0.0.1:<port> if GC_PPROF=1
// is set. The listener runs on a dedicated mux (not http.DefaultServeMux)
// and is returned so the caller can Shutdown it. Returns (nil, nil) when
// GC_PPROF is unset.
func StartPprof(addr string) (*http.Server, error) {
	if os.Getenv("GC_PPROF") != "1" {
		return nil, nil
	}
	if addr == "" {
		addr = "127.0.0.1:6060"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: addr, Handler: mux}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := srv.Serve(lis); err != nil && err != http.ErrServerClosed {
			log.Printf("pprof: %v", err)
		}
	}()
	log.Printf("pprof: listening on %s (GC_PPROF=1)", addr)
	return srv, nil
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
		problemCityNotFound.writeTo(w)
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

// buildMultiplexer creates a Multiplexer from all running cities'
// event providers plus any transient-city providers surfaced by a
// resolver that implements TransientCityEventSource. Including
// transient (pending init, in-progress, or failed) cities matters
// for clients that POST /v0/city and wait for city.created /
// city.ready / city.init_failed events on /v0/events/stream without
// polling — the city's own events.jsonl exists from Scaffold
// onward, but the city isn't in Running=true yet.
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
	if transient, ok := sm.resolver.(TransientCityEventSource); ok {
		for name, ep := range transient.TransientCityEventProviders() {
			if ep == nil {
				continue
			}
			mux.Add(name, ep)
		}
	}
	if supSrc, ok := sm.resolver.(SupervisorEventSource); ok {
		if rec := supSrc.SupervisorEventRecorder(); rec != nil {
			if prov, ok := rec.(events.Provider); ok {
				mux.Add("__supervisor__", prov)
			}
		}
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
