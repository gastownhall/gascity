package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rigstate"
)

func loadRigSuspensionState(fs fsys.FS, cityPath string) (rigstate.SuspensionState, error) {
	return rigstate.Load(fs, cityPath)
}

func saveRigSuspensionState(fs fsys.FS, cityPath string, st rigstate.SuspensionState) error {
	return rigstate.Save(fs, cityPath, st)
}

// suspendRigInState records an explicit "suspended" runtime preference
// for the rig. Returns false (no-op) if an explicit suspend was already
// in place.
func suspendRigInState(st *rigstate.SuspensionState, name string) bool {
	if v, ok := rigstate.ExplicitSuspended(*st, name); ok && v {
		return false
	}
	t := true
	rigstate.SetSuspended(st, name, &t)
	return true
}

// resumeRigInState records an explicit "resumed" runtime preference for
// the rig, ensuring suspended_on_start can't reassert across restarts.
// Returns false (no-op) if an explicit resume was already in place.
func resumeRigInState(st *rigstate.SuspensionState, name string) bool {
	if v, ok := rigstate.ExplicitSuspended(*st, name); ok && !v {
		return false
	}
	f := false
	rigstate.SetSuspended(st, name, &f)
	return true
}

// isRigSuspendedInState reports whether the runtime state explicitly
// suspends the rig. An explicit resume returns false; callers that
// want the effective state should use [buildEffectiveSuspendedRigNames]
// or [rigstate.EffectiveSuspended] with the rig's SuspendedOnStart.
func isRigSuspendedInState(st rigstate.SuspensionState, name string) bool {
	return rigstate.IsSuspended(st, name)
}

// buildEffectiveSuspendedRigNames returns the set of rig names whose
// effective state is suspended: the runtime override wins, otherwise
// the rig's committed SuspendedOnStart applies. The deprecated city.toml
// `suspended` field is intentionally NOT consulted — `gc doctor` flags
// it as a migration target.
func buildEffectiveSuspendedRigNames(cfg *config.City, rs rigstate.SuspensionState) map[string]bool {
	names := make(map[string]bool)
	for i := range cfg.Rigs {
		if rigstate.EffectiveSuspended(rs, cfg.Rigs[i].Name, cfg.Rigs[i].SuspendedOnStart) {
			names[cfg.Rigs[i].Name] = true
		}
	}
	return names
}

// buildMergedSuspendedRigNames is the back-compat alias for
// [buildEffectiveSuspendedRigNames]. Callers haven't all been renamed
// yet; both names exist while the runtime-state rollout settles.
func buildMergedSuspendedRigNames(cfg *config.City, rs rigstate.SuspensionState) map[string]bool {
	return buildEffectiveSuspendedRigNames(cfg, rs)
}
