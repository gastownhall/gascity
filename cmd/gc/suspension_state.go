package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// loadSuspensionState reads .gc/runtime/suspension-state.json. A
// missing file returns a zero-value State, mirroring the convention
// in [internal/suspensionstate].
func loadSuspensionState(fs fsys.FS, cityPath string) (suspensionstate.State, error) {
	return suspensionstate.Load(fs, cityPath)
}

// saveSuspensionState writes the runtime suspension state to disk.
func saveSuspensionState(fs fsys.FS, cityPath string, st suspensionstate.State) error {
	return suspensionstate.Save(fs, cityPath, st)
}

// loadSuspensionStateBestEffort returns the runtime suspension state,
// silently falling back to a zero state on any I/O error. Suitable
// for the suspension predicates where misclassifying as "not
// suspended" is no worse than the pre-existing behavior.
func loadSuspensionStateBestEffort(cityPath string) suspensionstate.State {
	if cityPath == "" {
		return suspensionstate.State{}
	}
	st, _ := suspensionstate.Load(fsys.OSFS{}, cityPath)
	return st
}

// suspendRigInState records an explicit "suspended" runtime
// preference for the rig. Returns false (no-op) when an explicit
// suspend is already in place.
func suspendRigInState(st *suspensionstate.State, name string) bool {
	if v, ok := suspensionstate.ExplicitRig(*st, name); ok && v {
		return false
	}
	t := true
	suspensionstate.SetRig(st, name, &t)
	return true
}

// resumeRigInState records an explicit "resumed" runtime preference
// for the rig, ensuring suspended_on_start can't reassert across
// restarts. Returns false (no-op) when an explicit resume is already
// in place.
func resumeRigInState(st *suspensionstate.State, name string) bool {
	if v, ok := suspensionstate.ExplicitRig(*st, name); ok && !v {
		return false
	}
	f := false
	suspensionstate.SetRig(st, name, &f)
	return true
}

// isRigSuspendedInState reports whether the runtime state explicitly
// suspends the rig. An explicit resume returns false; callers that
// want the effective state should use [buildEffectiveSuspendedRigNames]
// or [suspensionstate.EffectiveRigSuspended] with the rig's
// EffectiveSuspendedOnStart.
func isRigSuspendedInState(st suspensionstate.State, name string) bool {
	return suspensionstate.IsRigSuspended(st, name)
}

// buildEffectiveSuspendedRigNames returns the set of rig names whose
// effective state is suspended: the runtime override wins, otherwise
// the rig's authored default applies. The deprecated `[[rig]]
// suspended` field is treated as an alias for `suspended_on_start`
// via [config.Rig.EffectiveSuspendedOnStart], so existing city.toml
// files with `suspended = true` continue to start rigs suspended.
func buildEffectiveSuspendedRigNames(cfg *config.City, st suspensionstate.State) map[string]bool {
	names := make(map[string]bool)
	if cfg == nil {
		return names
	}
	for i := range cfg.Rigs {
		r := &cfg.Rigs[i]
		if suspensionstate.EffectiveRigSuspended(st, r.Name, r.EffectiveSuspendedOnStart()) {
			names[r.Name] = true
		}
	}
	return names
}

// pruneRemovedRigSuspensionOverrides drops runtime suspend/resume overrides
// for rigs the live config no longer declares, so a rig deregistered by
// hand-editing city.toml cannot pin a later rig of the same name against its
// suspended_on_start.
//
// It only trusts a config the runtime has fully loaded and is running: a
// failed reload keeps the old config, and a config change observed during
// the reload means the view may already be stale (the dirty flag triggers a
// fresh reload that prunes instead). Rigs lacking a site.toml path binding
// stay in cfg.Rigs with an empty path, so they count as declared.
func (cr *CityRuntime) pruneRemovedRigSuspensionOverrides(reply reloadControlReply) {
	if reply.Outcome != reloadOutcomeApplied && reply.Outcome != reloadOutcomeNoChange {
		return
	}
	if cr.cfg == nil || cr.cityPath == "" {
		return
	}
	if cr.configDirty != nil && cr.configDirty.Load() {
		return
	}
	declared := make(map[string]bool, len(cr.cfg.Rigs))
	for _, r := range cr.cfg.Rigs {
		declared[r.Name] = true
	}
	pruned, err := suspensionstate.PruneRigsNotIn(fsys.OSFS{}, cr.cityPath, declared)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: pruning runtime suspension state: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	for _, name := range pruned {
		fmt.Fprintf(cr.stderr, "%s: cleared runtime suspension override for rig %q (no longer declared in config)\n", cr.logPrefix, name) //nolint:errcheck // best-effort stderr
	}
}
