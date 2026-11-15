package main

import (
	"io"
	"path/filepath"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// bdPassthroughReadinessFallbacks counts passthrough loads whose first
// composition failed and that therefore ran the full builtin readiness pass
// before composing again (test seam).
var bdPassthroughReadinessFallbacks atomic.Int64

// loadCityConfigForBdPassthrough is the gc bd passthrough's one config load.
//
// It composes from the builtin pack caches as they are on disk, without the
// readiness pass loadCityConfigFS runs first. That pass cannot change what the
// passthrough routes on:
//
//   - No routing input can come from builtin pack content. A pack cannot
//     declare rigs, [beads], [storage] or the workspace prefix; the
//     passthrough routes on those, .gc/site.toml, the env and argv.
//   - Composition already rebuilds an ABSENT bundled cache offline (the
//     binary-upgrade case), and rejects one whose marker is missing or wrong.
//   - The long-lived processes and every other gc command keep the caches and
//     the gc-beads-bd shim healed.
//
// The one case where the pass changes the outcome is a cache composition
// rejects. Then this runs the pass and composes again, so the answer and the
// error text are exactly loadCityConfig's. Only a composition error falls
// back: a validation error after a successful composition is returned as is,
// without a pointless pass or a second round of warnings.
//
// On success without a pass it records the deferral in the invocation scope,
// so the rest of the invocation (the write guard's store open, any further
// full load) does not run the pass either.
//
// It also skips warnMissingRequiredBuiltinImports: that is a config-health
// hint gc status, gc start and gc doctor print, not something a data-plane
// passthrough agents call thousands of times a day should repeat.
func loadCityConfigForBdPassthrough(cityPath string, warnings io.Writer) (*config.City, error) {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, prov, err := loadComposedCityConfig(fsys.OSFS{}, tomlPath, skipRevisionSnapshot)
	if err != nil {
		bdPassthroughReadinessFallbacks.Add(1)
		if ensureErr := EnsureBuiltinRuntimeAssets(cityPath, warnings); ensureErr != nil {
			// What loadCityConfigFS would have returned first.
			return nil, ensureErr
		}
		cfg, prov, err = loadComposedCityConfig(fsys.OSFS{}, tomlPath, skipRevisionSnapshot)
		if err != nil {
			return nil, err
		}
	} else {
		deferBuiltinReadinessForInvocation(cityPath)
	}
	emitLoadCityConfigWarnings(warnings, prov)
	if err := validatePackRuntimeRegistrations(cfg); err != nil {
		return nil, err
	}
	applyFeatureFlags(cfg)
	return cfg, nil
}
