package api

import (
	"context"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// CityInfo describes a managed city for the /v0/cities endpoint.
type CityInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Running bool   `json:"running"`
	Status  string `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
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
type SupervisorMux struct {
	resolver  CityResolver
	readOnly  bool
	version   string
	startedAt time.Time
	server    *http.Server

	// Per-city Server cache. Keyed by city name. Invalidated when
	// the State pointer changes (city restarted → new controllerState).
	cacheMu sync.RWMutex
	cache   map[string]cachedCityServer
}

// NewSupervisorMux creates a SupervisorMux that routes requests to cities
// resolved by the given CityResolver.
func NewSupervisorMux(resolver CityResolver, readOnly bool, version string, startedAt time.Time) *SupervisorMux {
	sm := &SupervisorMux{
		resolver:  resolver,
		readOnly:  readOnly,
		version:   version,
		startedAt: startedAt,
		cache:     make(map[string]cachedCityServer),
	}
	sm.server = &http.Server{Handler: sm.Handler()}
	return sm
}

// Handler returns an http.Handler with the standard middleware chain applied.
func (sm *SupervisorMux) Handler() http.Handler {
	apiInner := withCSRFCheck(http.HandlerFunc(sm.ServeHTTP))
	if sm.readOnly {
		apiInner = withReadOnly(apiInner)
	}
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if supervisorServicePath(r.URL.Path) {
			// Workspace services apply their own publication and CSRF rules
			// in the per-city server. Do not impose supervisor API policy on
			// top of service mounts.
			sm.ServeHTTP(w, r)
			return
		}
		apiInner.ServeHTTP(w, r)
	})
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

	// Supervisor-level endpoints.
	if path == "/v0/cities" && r.Method == http.MethodGet {
		sm.handleCities(w, r)
		return
	}
	if path == "/v0/provider-readiness" && r.Method == http.MethodGet {
		handleProviderReadiness(w, r)
		return
	}
	if path == "/v0/readiness" && r.Method == http.MethodGet {
		handleReadiness(w, r)
		return
	}
	if path == "/health" && r.Method == http.MethodGet {
		sm.handleHealth(w, r)
		return
	}
	if path == "/v0/events/stream" && r.Method == http.MethodGet {
		sm.handleGlobalEventStream(w, r)
		return
	}
	if path == "/v0/events" && r.Method == http.MethodGet {
		sm.handleGlobalEventList(w, r)
		return
	}

	// City creation is supervisor-level: it shells out to `gc init` and
	// doesn't need an existing running city, so handle it before the
	// per-city and backward-compat routing below.
	if path == "/v0/city" && r.Method == http.MethodPost {
		if sm.readOnly {
			writeError(w, http.StatusForbidden, "read_only", "mutations disabled: server bound to non-localhost address")
			return
		}
		handleCityCreate(w, r)
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
			writeError(w, http.StatusBadRequest, "bad_request", "city name required in URL")
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
			writeError(w, http.StatusServiceUnavailable, "no_cities", "no cities running")
		case 1:
			sm.serveCityRequest(w, r, running[0].Name, path)
		default:
			writeError(w, http.StatusBadRequest, "city_required",
				"multiple cities running; use /v0/city/{name}/... to specify which city")
		}
		return
	}

	http.NotFound(w, r)
}

// serveCityRequest resolves a city's State and dispatches to a per-city Server.
func (sm *SupervisorMux) serveCityRequest(w http.ResponseWriter, r *http.Request, cityName, path string) {
	state := sm.resolver.CityState(cityName)
	if state == nil {
		// Evict stale cache entry if the city is gone.
		sm.cacheMu.Lock()
		delete(sm.cache, cityName)
		sm.cacheMu.Unlock()
		writeError(w, http.StatusNotFound, "not_found", "city not found or not running: "+cityName)
		return
	}

	srv := sm.getCityServer(cityName, state)

	// Rewrite the request path to the per-city route.
	r2 := r.Clone(r.Context())
	r2.URL.Path = path
	r2.URL.RawPath = ""
	// Dispatch through the mux directly — middleware is applied at the SupervisorMux level.
	srv.mux.ServeHTTP(w, r2)
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

func (sm *SupervisorMux) handleCities(w http.ResponseWriter, _ *http.Request) {
	cities := sm.resolver.ListCities()
	sort.Slice(cities, func(i, j int) bool { return cities[i].Name < cities[j].Name })
	writeJSON(w, http.StatusOK, listResponse{Items: cities, Total: len(cities)})
}

// handleGlobalEventStream streams SSE events from all running cities,
// tagged with city name. The cursor format for reconnection is
// "city1:seq1,city2:seq2" via Last-Event-ID or ?after_cursor.
func (sm *SupervisorMux) handleGlobalEventStream(w http.ResponseWriter, r *http.Request) {
	mux := sm.buildMultiplexer()

	// Parse cursor from Last-Event-ID or query param.
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after_cursor")
	}
	cursors := events.ParseCursor(cursor)

	mw, err := mux.Watch(r.Context(), cursors)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "internal", "failed to start global event watcher: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if err := http.NewResponseController(w).Flush(); err != nil {
		_ = err
	}

	// Stream tagged events with composite cursor IDs. We use a
	// dedicated loop because the SSE id must be a composite per-city
	// cursor, not a scalar Seq.
	streamProjectedGlobalEvents(r.Context(), w, mw, cursors, sm.resolver)
}

// handleGlobalEventList returns events from all running cities, sorted
// by timestamp, with each event tagged with its source city.
func (sm *SupervisorMux) handleGlobalEventList(w http.ResponseWriter, r *http.Request) {
	mux := sm.buildMultiplexer()

	q := r.URL.Query()
	filter := events.Filter{
		Type:  q.Get("type"),
		Actor: q.Get("actor"),
	}
	if v := q.Get("since"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			filter.Since = time.Now().Add(-d)
		}
	}

	evts, err := mux.ListAll(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if evts == nil {
		evts = []events.TaggedEvent{}
	}
	writeJSON(w, http.StatusOK, listResponse{Items: evts, Total: len(evts)})
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

func (sm *SupervisorMux) handleHealth(w http.ResponseWriter, _ *http.Request) {
	cities := sm.resolver.ListCities()
	var running int
	for _, c := range cities {
		if c.Running {
			running++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"version":        sm.version,
		"uptime_sec":     int(time.Since(sm.startedAt).Seconds()),
		"cities_total":   len(cities),
		"cities_running": running,
	})
}
