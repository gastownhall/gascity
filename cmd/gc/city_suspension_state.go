package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/citystate"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// loadCitySuspensionState reads the runtime city state. A missing file
// returns a zero-value State, mirroring [rigstate.Load].
func loadCitySuspensionState(fs fsys.FS, cityPath string) (citystate.State, error) {
	return citystate.Load(fs, cityPath)
}

// effectiveCitySuspended is the canonical "is the city suspended right
// now" predicate. It honors the GC_SUSPENDED=1 escape hatch (used by
// integration tests and ops to override without touching files), then
// the runtime state file, then falls back to the workspace's
// SuspendedOnStart committable default. The deprecated
// [workspace] suspended field is intentionally NOT consulted —
// `gc doctor` flags it as a migration target.
func effectiveCitySuspended(cfg *config.City, st citystate.State) bool {
	if os.Getenv("GC_SUSPENDED") == "1" {
		return true
	}
	if cfg == nil {
		return citystate.EffectiveSuspended(st, false)
	}
	return citystate.EffectiveSuspended(st, cfg.Workspace.SuspendedOnStart)
}

// effectiveCitySuspendedFromFS is the common-path helper that loads
// runtime state from disk and computes the effective suspended state.
// Any I/O error reading the state file produces a "treat as zero
// state" result: callers that need stricter handling should call
// [loadCitySuspensionState] directly.
func effectiveCitySuspendedFromFS(fs fsys.FS, cityPath string, cfg *config.City) bool {
	st, _ := loadCitySuspensionState(fs, cityPath)
	return effectiveCitySuspended(cfg, st)
}
