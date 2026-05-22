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

// apiClient returns an API client if a controller with a mutable API server
// is running for the city at cityPath. Returns nil if no controller is running,
// the API is not configured, GC_NO_API is set truthy (operator escape hatch),
// or the API is bound to a non-localhost address without allow_mutations.
// CLI commands use this to route reads/writes through the API when available,
// falling back to direct bd or file mutation.
func apiClient(cityPath string) *api.Client {
	// Operator escape hatch: GC_NO_API=1|true|yes → always fall back.
	// Unknown values warn to stderr and fail open (fall through to normal path).
	if disabled, warn := classifyGCNoAPI(os.Getenv("GC_NO_API")); disabled {
		return nil
	} else if warn != "" {
		fmt.Fprintln(os.Stderr, "warning: "+warn) //nolint:errcheck // best-effort stderr
	}
	// Check if controller is alive.
	if controllerAlive(cityPath) != 0 {
		// Load config to find API port.
		tomlPath := filepath.Join(cityPath, "city.toml")
		cfg, err := config.Load(fsys.OSFS{}, tomlPath)
		if err != nil {
			return nil
		}
		if cfg.API.Port <= 0 {
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
	return supervisorCityAPIClient(cityPath)
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
		return name
	}
	resolved, ok := resolveAgentIdentity(cfg, name, currentRigContext(cfg))
	if !ok {
		return name
	}
	return resolved.QualifiedName()
}
