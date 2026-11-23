package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
)

// resolveScopeProxiedIdleTimeout resolves the idle timeout bd should persist
// for scopeRoot's proxied store when gc initializes or migrates it.
//
// A city with no city.toml yet resolves the default (and the environment
// override). A rig whose scope shares the city's proxy root resolves the
// city's value: whichever scope's bd spawns a shared proxy uses its own
// sidecar's value, so every scope on the root must carry the same one. A rig
// override on such a rig is ignored with a warning on warn.
func resolveScopeProxiedIdleTimeout(cityPath, scopeRoot string, warn io.Writer) (config.ProxiedIdleTimeout, error) {
	cfg, err := loadCityConfigForProxiedIdleTimeout(cityPath)
	if err != nil {
		return config.ProxiedIdleTimeout{}, err
	}
	return proxiedIdleTimeoutForScope(cfg, cityPath, scopeRoot, warn)
}

// proxiedIdleTimeoutForScope is resolveScopeProxiedIdleTimeout over an
// already-loaded config. cfg may be nil.
func proxiedIdleTimeoutForScope(cfg *config.City, cityPath, scopeRoot string, warn io.Writer) (config.ProxiedIdleTimeout, error) {
	var rig *config.Rig
	if cfg != nil && !samePath(cityPath, scopeRoot) {
		rig = rigConfigForScopeRoot(cityPath, scopeRoot, cfg.Rigs)
	}
	shares := rig != nil && proxyendpoint.SharesCityRoot(cityPath, scopeRoot)
	idle, ignored, err := config.ProxiedIdleTimeoutForScope(cfg, rig, shares)
	if ignored && warn != nil {
		fmt.Fprintf(warn, "warning: %s\n", ignoredSharedRootIdleOverride(rig.Name)) //nolint:errcheck // best-effort warning
	}
	return idle, err
}

// ignoredSharedRootIdleOverride is the warning for a shared-root rig that sets
// its own idle timeout.
func ignoredSharedRootIdleOverride(rigName string) string {
	return fmt.Sprintf("rig %q beads_proxied_idle_timeout is ignored: the rig shares the city's proxy root, so it uses the city's [beads] proxied_idle_timeout", rigName)
}

// loadCityConfigForProxiedIdleTimeout loads the city config, answering nil for
// a city that has no city.toml yet. Any other load failure is returned: an
// unreadable config must not silently become the default idle policy.
func loadCityConfigForProxiedIdleTimeout(cityPath string) (*config.City, error) {
	if cityPath == "" {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	// No builtin pack refresh: resolving a value is a read.
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("resolve proxied idle timeout: %w", err)
	}
	return cfg, nil
}
