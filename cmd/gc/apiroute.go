package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Indirected for tests. apiClient's branch between a standalone controller
// (which binds cfg.API.Port) and a supervisor-managed city (which is reached
// on the supervisor's port via city-scoped routes) depends on the hosting mode
// the live per-city controller socket reports and on whether the supervisor
// requires city grants, so tests swap these to exercise each route without a
// running controller or supervisor. (gascity ga-tp7)
var (
	apiRouteControllerIdentityHook = probeControllerIdentity
	apiRouteSupervisorClientHook   = supervisorCityAPIClient
	apiRouteSupervisorGrantsHook   = supervisorRequiresCityGrants
)

// apiClient returns an API client if a controller with a mutable API server
// is running for the city at cityPath. Returns nil if no controller is running,
// the API is not configured, GC_NO_API is set truthy (operator escape hatch),
// or the API is bound to a non-localhost address without allow_mutations.
// CLI commands use this to route reads/writes through the API when available,
// falling back to direct bd or file mutation.
//
// A standalone controller (gc controller / gc serve) and a supervisor-managed
// city both answer the per-city controller socket — the supervisor hosts that
// controller in-process — so apiClient routes on the hosting mode the socket
// reports through identify. A supervisor-hosted city gets the
// supervisor-managed client: the supervisor ignores the city's [api] port and
// serves it on its own port, and a process-owned runtime (such as an ACP
// session) lives only in the supervisor's memory, out of reach of a local
// fallback. When that supervisor requires a signed grant on city routes
// (supervisorRequiresCityGrants), apiClient returns nil instead: this client
// mints no grant, so the supervisor would refuse the gated calls with a 401
// that no caller falls back from, and apiClient cannot tell a caller's read
// from its write. A standalone controller, or one predating identify (hosting
// mode unknown), gets its [api] port's HTTP endpoint, or nil when none is
// usable so the caller uses its local fallback. With no live socket apiClient
// returns the supervisor-managed client. Callers whose local fallback is
// missing or prohibitively expensive use supervisorFallthroughAPIClient
// instead. (gascity ga-tp7, ra-r9hm6v, #5262)
func apiClient(cityPath string) *api.Client {
	return routeAPIClient(cityPath, apiRouteGeneral)
}

// supervisorFallthroughAPIClient resolves an API client the way apiClient
// does, for callers whose local fallback is missing or prohibitively
// expensive: maintenance commands have no local fallback at all, and
// `gc status`'s local fallback re-opens the full local bead/dolt store and
// rescans event archives to rebuild store health — far more expensive than
// the supervisor's already-cached response (ra-r9hm6v: ~9.5s local CPU vs
// ~0.35s warm API). Where apiClient returns nil for its caller's local
// fallback, it routes to the supervisor-managed client instead in two shapes:
// a supervisor-hosted city whose supervisor requires grants (these callers
// handle a refusal as they handle any other API error), and a live controller
// of unknown hosting mode without a usable [api] port — the shape of a
// supervisor-managed city whose supervisor predates identify. A controller
// that identifies as standalone serves only its own [api] port, so it is
// never sent to the supervisor, and each call resolves the supervisor-managed
// client at most once.
func supervisorFallthroughAPIClient(cityPath string) (*api.Client, string) {
	if c := routeAPIClient(cityPath, apiRoutePreferSupervisor); c != nil {
		return c, ""
	}
	return nil, routeFallbackReason(cityPath, apiRoutePreferSupervisor)
}

// localWhenSupervisorHostedAPIClient resolves an API client the way apiClient
// does, for a read whose local path renders the command's own JSON contract
// from on-disk state, except that a supervisor-hosted city gets nil with
// reason "supervisor-hosted", so its caller reads locally. `gc session list`
// is that read: its local rows carry the fields
// schemas/session/list/result.schema.json defines (agent_name, closed, the
// summary), which the API's session rows do not, and the API fills each active
// session's active bead by reading every rig store, a suspended rig's
// included. Pool-slot lookups parse agent_name, and the core orphan-sweep
// order lists sessions in cities whose rigs may be suspended.
func localWhenSupervisorHostedAPIClient(cityPath string) (*api.Client, string) {
	if c := routeAPIClient(cityPath, apiRouteLocalWhenSupervisorHosted); c != nil {
		return c, ""
	}
	return nil, routeFallbackReason(cityPath, apiRouteLocalWhenSupervisorHosted)
}

// apiRoute selects how routeAPIClient routes the shapes in which its callers
// differ.
type apiRoute int

const (
	// apiRouteGeneral is apiClient's routing.
	apiRouteGeneral apiRoute = iota
	// apiRoutePreferSupervisor is supervisorFallthroughAPIClient's routing: the
	// supervisor-managed client in the shapes where apiClient returns nil for
	// its caller's local fallback.
	apiRoutePreferSupervisor
	// apiRouteLocalWhenSupervisorHosted is localWhenSupervisorHostedAPIClient's
	// routing: nil for a supervisor-hosted city.
	apiRouteLocalWhenSupervisorHosted
)

// routeAPIClient is the routing ladder apiClient,
// supervisorFallthroughAPIClient and localWhenSupervisorHostedAPIClient share;
// route selects which of them it resolves for.
func routeAPIClient(cityPath string, route apiRoute) *api.Client {
	// Remote routing is NOT handled here. A remote target is refused upstream by
	// the capability gate in resolveContext (Phase 1) and, once enabled, will be
	// served by a resolution-aware remote transport keyed on
	// resolvedContext.Remote (Phase 2) — never by sniffing global flags/env in
	// this local loopback ladder. Sniffing here is wrong: a local --city command
	// that merely has a stray GC_CITY_URL in its environment resolves LOCAL (flag
	// beats env), and must still route through its live local controller.
	// GC_NO_API + a resolved remote target is already a loud error at resolution
	// (guardNoAPI), so no remote op can reach the GC_NO_API nil-return below.
	//
	// Operator escape hatch: GC_NO_API=1|true|yes → always fall back.
	// Unknown values warn to stderr and fail open (fall through to normal path).
	if disabled, warn := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return nil
	} else if warn != "" {
		fmt.Fprintln(os.Stderr, "warning: "+warn) //nolint:errcheck // best-effort stderr
	}
	identity := apiRouteControllerIdentityHook(cityPath)
	switch {
	case identity.PID == 0:
		// No controller socket: the supervisor-managed client, nil unless the
		// supervisor runs the city.
		return apiRouteSupervisorClientHook(cityPath)
	case identity.HostingMode == controllerHostingSupervisor:
		// The supervisor hosts the city in-process and serves it on its own
		// port via city-scoped routes, ignoring [api] port.
		switch route {
		case apiRouteLocalWhenSupervisorHosted:
			return nil
		case apiRouteGeneral:
			if apiRouteSupervisorGrantsHook() {
				return nil
			}
		}
		return apiRouteSupervisorClientHook(cityPath)
	}
	// Standalone controller, or one predating identify (hosting mode
	// unknown): its configured [api] port is the only endpoint it serves.
	c := standaloneControllerClient(cityPath)
	if c == nil && route == apiRoutePreferSupervisor && identity.HostingMode == controllerHostingUnknown {
		return apiRouteSupervisorClientHook(cityPath)
	}
	return c
}

// standaloneControllerClient builds an API client for a standalone controller
// that binds cfg.API.Port from city.toml. It returns nil, which apiClient hands
// back to its caller, when no usable standalone HTTP endpoint exists: the
// config is unreadable, [api] port is unset, or the listener binds a
// non-loopback address without allow_mutations.
func standaloneControllerClient(cityPath string) *api.Client {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := config.Load(fsys.OSFS{}, tomlPath)
	if err != nil || cfg.API.Port <= 0 {
		return nil
	}
	// Non-localhost bind means API runs read-only — skip API routing
	// (unless allow_mutations is set).
	bind := cfg.API.BindOrDefault()
	if bind != "127.0.0.1" && bind != "localhost" && bind != "::1" && !cfg.API.AllowMutations {
		return nil
	}
	baseURL := fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port)))
	// Standalone controller serves /v0/city/{cityName}/... routes via
	// api.NewSupervisorMux, so per-city method calls need a city-scoped
	// client. Derive the city name from config; the controller only
	// serves one city in standalone mode.
	return api.NewCityScopedClient(baseURL, standaloneControllerCityName(cfg, cityPath))
}

// standaloneControllerCityName resolves the effective city name for a
// standalone controller API client. In standalone mode the controller serves
// exactly one city, so the client must match the runtime identity.
func standaloneControllerCityName(cfg *config.City, cityPath string) string {
	return loadedCityName(cfg, cityPath)
}

// apiClientFallbackReason returns a reason code describing why apiClient
// returned nil for cityPath. Read-path CLI commands call this when the
// client is nil to emit a route=fallback reason=<code> log line.
//
// The closed set mirrors the enabler's reason codes (ga-71l): "escape-hatch"
// (GC_NO_API truthy), "non-loopback-bind" (a standalone controller's API bound
// to non-localhost with mutations disallowed), "grant-required" (a
// supervisor-hosted city whose supervisor requires city grants),
// "controller-down" (everything else — no controller, config missing, API
// port unset, no supervisor-managed client).
func apiClientFallbackReason(cityPath string) string {
	return routeFallbackReason(cityPath, apiRouteGeneral)
}

// routeFallbackReason returns the apiClientFallbackReason code for a nil from
// routeAPIClient with the same route, or "supervisor-hosted" for a
// supervisor-hosted city on localWhenSupervisorHostedAPIClient's route. A
// supervisor-hosted city's [api] bind is never the reason: the supervisor
// ignores that port.
func routeFallbackReason(cityPath string, route apiRoute) string {
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return "escape-hatch"
	}
	identity := apiRouteControllerIdentityHook(cityPath)
	switch {
	case identity.PID == 0:
		// No controller socket, and no supervisor-managed client either.
	case identity.HostingMode == controllerHostingSupervisor:
		switch route {
		case apiRouteLocalWhenSupervisorHosted:
			return "supervisor-hosted"
		case apiRouteGeneral:
			if apiRouteSupervisorGrantsHook() {
				return "grant-required"
			}
		}
	default:
		tomlPath := filepath.Join(cityPath, "city.toml")
		if cfg, err := config.Load(fsys.OSFS{}, tomlPath); err == nil && cfg.API.Port > 0 {
			bind := cfg.API.BindOrDefault()
			if bind != "127.0.0.1" && bind != "localhost" && bind != "::1" && !cfg.API.AllowMutations {
				return "non-loopback-bind"
			}
		}
	}
	return "controller-down"
}

// resolveAgentForAPI resolves a bare agent name (e.g., "worker") to its
// qualified form (e.g., "myrig/worker") using the current rig context, so
// the API server can find the agent. If already qualified or resolution
// fails, the original name is returned.
func resolveAgentForAPI(cityPath, name string) string {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg, err = loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
		if err != nil {
			return name
		}
	}
	resolved, ok := resolveAgentIdentity(cfg, name, currentRigContext(cfg))
	if !ok {
		return name
	}
	return resolved.QualifiedName()
}
