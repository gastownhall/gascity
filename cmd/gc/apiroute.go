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
// on the supervisor's port via city-scoped routes) depends on live sockets,
// so tests swap these to exercise the fall-through without a running
// controller or supervisor. (gascity ga-tp7)
var (
	apiRouteControllerAliveHook  = controllerAlive
	apiRouteSupervisorClientHook = supervisorCityAPIClient
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
// controller in-process. When the socket is alive, apiClient routes to the
// standalone HTTP endpoint if the city configures an [api] port, otherwise
// returns nil so the caller uses its local fallback; when the socket is not
// alive it returns the supervisor-managed client. Maintenance commands have no
// local fallback, so they use maintenanceAPIClient, which additionally routes a
// supervisor-managed city (alive socket, no standalone [api] port) to the
// supervisor client rather than reporting controller-down. (gascity ga-tp7)
func apiClient(cityPath string) *api.Client {
	// Operator escape hatch: GC_NO_API=1|true|yes → always fall back.
	// Unknown values warn to stderr and fail open (fall through to normal path).
	if disabled, warn := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return nil
	} else if warn != "" {
		fmt.Fprintln(os.Stderr, "warning: "+warn) //nolint:errcheck // best-effort stderr
	}
	if apiRouteControllerAliveHook(cityPath) != 0 {
		// Alive socket: use the standalone HTTP endpoint when configured, else
		// return nil so the caller takes its local fallback. A supervisor-managed
		// city (no standalone [api] port) reaches the supervisor client only via
		// maintenanceAPIClient, which has no local fallback.
		return standaloneControllerClient(cityPath)
	}
	return apiRouteSupervisorClientHook(cityPath)
}

// standaloneControllerClient builds an API client for a standalone controller
// that binds cfg.API.Port from city.toml. It returns nil — the signal for
// apiClient to fall through to the supervisor-managed client — when no usable
// standalone HTTP endpoint exists: the config is unreadable, [api] port is
// unset, or the listener binds a non-loopback address without allow_mutations.
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
// (GC_NO_API truthy), "non-loopback-bind" (API bound to non-localhost with
// mutations disallowed), "controller-down" (everything else — no controller,
// config missing, API port unset).
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
