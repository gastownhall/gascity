package main

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// apiClient returns an API client if a controller with a mutable API server
// is running for the city at cityPath. Returns nil if no controller is running,
// the API is not configured, or the API is bound to a non-localhost address
// (which runs in read-only mode). CLI commands use this to route writes through
// the API when available, falling back to direct file mutation.
func apiClient(cityPath string) *api.Client {
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

// standaloneControllerCityName resolves the city name for a standalone
// controller API client. In standalone mode the controller serves exactly
// one city; we prefer cfg.Workspace.Name when set, falling back to the
// resolved name from the city directory path.
func standaloneControllerCityName(cfg *config.City, cityPath string) string {
	if cfg != nil && cfg.Workspace.Name != "" {
		return cfg.Workspace.Name
	}
	return resolveCityName("", "", cityPath)
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
