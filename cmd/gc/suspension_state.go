package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/citylayout"
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

// updateRigSuspension applies apply (suspendRigInState or
// resumeRigInState) to the rig through [suspensionstate.Update], so the
// read and the write hold the suspension fence together, and reports
// whether apply changed the state. An unchanged state is not rewritten.
func updateRigSuspension(fs fsys.FS, cityPath, rigName string, apply func(*suspensionstate.State, string) bool) (bool, error) {
	changed := false
	err := suspensionstate.Update(fs, cityPath, func(st *suspensionstate.State) bool {
		changed = apply(st, rigName)
		return changed
	})
	return changed, err
}

// readSuspensionForWake reads the runtime suspension state for a controller
// pass that decides starts. A missing file is the zero State with no error. An
// unreadable or unparseable file returns the zero State with an error naming
// the file, so the caller holds starts instead of reading "not suspended".
// The pass decides its drains from suspensionForPass, which suspends nothing
// on that error.
func readSuspensionForWake(cityPath string) (suspensionstate.State, error) {
	if cityPath == "" {
		return suspensionstate.State{}, nil
	}
	st, err := suspensionstate.Load(fsys.OSFS{}, cityPath)
	if err != nil {
		return suspensionstate.State{}, fmt.Errorf("reading %s: %w", citylayout.SuspensionStateFile(cityPath), err)
	}
	return st, nil
}

// suspensionForPass is the suspension state a reconciler pass decides its
// desired sessions, sleeps and drains from, given its read of the file: st
// when the read succeeded. After a failed read the pass cannot tell what the
// file holds, and the zero State would fall back to the authored
// suspended_on_start defaults, draining a city or rig the operator resumed.
// So it is an explicit resume of the city and of every rig in cfg instead,
// which suspends nothing the file decides while GC_SUSPENDED and agent-level
// suspension still apply. The pass holds every start and create on that
// error (decideStartHold), so an unreadable file drains nothing and wakes
// nothing.
func suspensionForPass(cfg *config.City, st suspensionstate.State, readErr error) suspensionstate.State {
	if readErr == nil {
		return st
	}
	resumed := func() suspensionstate.Override {
		no := false
		return suspensionstate.Override{Suspended: &no}
	}
	view := suspensionstate.State{City: resumed()}
	if cfg != nil && len(cfg.Rigs) > 0 {
		view.Rigs = make(map[string]suspensionstate.Override, len(cfg.Rigs))
		for i := range cfg.Rigs {
			view.Rigs[cfg.Rigs[i].Name] = resumed()
		}
	}
	return view
}

// loadSuspensionStateForPass reads the runtime suspension state a legacy
// reconciler pass decides its desired sessions, sleeps and drains from: the
// file as read, or after a failed read suspensionForPass's state, which
// suspends nothing. The pass's start hold reads the same file through
// readSuspensionForWake and holds every start on that error.
func loadSuspensionStateForPass(cfg *config.City, cityPath string) suspensionstate.State {
	if cityPath == "" {
		return suspensionstate.State{}
	}
	st, err := loadSuspensionState(fsys.OSFS{}, cityPath)
	return suspensionForPass(cfg, st, err)
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
