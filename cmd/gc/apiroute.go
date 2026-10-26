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
// on the supervisor's port via city-scoped routes) depends on how the live
// controller socket identifies itself, so tests swap these to exercise the
// routing without a running controller or supervisor. (gascity ga-tp7)
var (
	apiRouteControllerIdentityHook = probeControllerIdentity
	apiRouteSupervisorClientHook   = supervisorCityAPIClient
)

// apiClient returns an API client if a controller with a mutable API server
// is running for the city at cityPath. Returns nil if no controller is running,
// the city is not served by a supervisor, the API is not configured, GC_NO_API
// is set truthy (operator escape hatch), or the API is bound to a non-localhost
// address without allow_mutations. CLI commands use this to route
// reads/writes through the API when available, falling back to direct bd or
// file mutation.
//
// A standalone controller (gc controller / gc serve) and a supervisor-managed
// city both answer the per-city controller socket — the supervisor hosts that
// controller in-process — so liveness alone cannot tell the two apart; the
// socket's identify reply can. Supervisor mode ignores the city's [api] port
// and serves the city on the supervisor's own port via city-scoped routes, so
// for a supervisor-hosted city apiClient returns the supervisor-managed
// client — nil when none is available (the caller's local fallback, which can
// only reach state on disk, never the supervisor's in-memory sessions; #5262).
// A standalone controller, or one predating identify (hosting mode unknown),
// keeps the historical routing: the standalone HTTP endpoint when the city
// configures an [api] port, otherwise nil so the caller uses its local
// fallback. Callers whose local fallback is missing or prohibitively
// expensive use supervisorFallthroughAPIClient instead: maintenance (no local
// fallback at all) and gc status (local fallback re-opens the bead/dolt
// store). (gascity ga-tp7, ra-r9hm6v)
func apiClient(cityPath string) *api.Client {
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
	if identity.PID != 0 && identity.HostingMode != controllerHostingSupervisor {
		// Standalone controller, or one predating identify (hosting mode
		// unknown): its configured [api] port is the only endpoint it serves.
		return standaloneControllerClient(cityPath)
	}
	// No controller socket, or the supervisor hosts the city in-process and
	// serves it on its own port via city-scoped routes, ignoring [api] port.
	return apiRouteSupervisorClientHook(cityPath)
}

// standaloneControllerClient builds an API client for a standalone controller
// that binds cfg.API.Port from city.toml. It returns nil — which apiClient
// hands to its caller as the local-fallback signal — when no usable standalone
// HTTP endpoint exists: the config is unreadable, [api] port is unset, or the
// listener binds a non-loopback address without allow_mutations.
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

// supervisorFallthroughAPIClient resolves an API client the same way apiClient
// does, but additionally falls through to the supervisor-managed client when
// the per-city controller socket is alive with no usable standalone [api]
// port — a standalone or legacy (pre-identify) controller whose city is
// nonetheless served by an older supervisor. A supervisor-hosted city never
// needs this branch: apiClient has already resolved its supervisor client.
// That extra step serves callers whose local fallback is missing or
// prohibitively expensive: maintenance commands have no local fallback at
// all, and `gc status`'s local fallback re-opens the full local bead/dolt
// store and rescans event archives to rebuild store health — far more
// expensive than the supervisor's already-cached response (ra-r9hm6v: ~9.5s
// local CPU vs ~0.35s warm API). Such callers should route through here
// instead of apiClient directly.
func supervisorFallthroughAPIClient(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	// Honor the GC_NO_API escape hatch: apiClient returns nil under it, and the
	// identity probe/supervisor client below never re-check it, so without this
	// guard an explicit operator opt-out would be silently bypassed here.
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); !disabled {
		if apiRouteControllerIdentityHook(cityPath).PID != 0 {
			if c := apiRouteSupervisorClientHook(cityPath); c != nil {
				return c, ""
			}
		}
	}
	return nil, apiClientFallbackReason(cityPath)
}

// apiClientFallbackReason returns a reason code describing why apiClient
// returned nil for cityPath. Read-path CLI commands call this when the
// client is nil to emit a route=fallback reason=<code> log line.
//
// The closed set mirrors the enabler's reason codes (ga-71l): "escape-hatch"
// (GC_NO_API truthy), "non-loopback-bind" (API bound to non-localhost with
// mutations disallowed), "controller-down" (everything else — no controller,
// config missing, API port unset). Note that "non-loopback-bind" is no longer
// reachable for supervisorFallthroughAPIClient callers, which try the
// supervisor client before asking for a reason.
func apiClientFallbackReason(cityPath string) string {
	if disabled, _ := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return "escape-hatch"
	}
	if controllerAlive(cityPath) != 0 {
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
