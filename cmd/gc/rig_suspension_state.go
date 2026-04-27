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

func suspendRigInState(st *rigstate.SuspensionState, name string) bool {
	if rigstate.IsSuspended(*st, name) {
		return false
	}
	rigstate.SetSuspended(st, name, true)
	return true
}

func resumeRigInState(st *rigstate.SuspensionState, name string) bool {
	if !rigstate.IsSuspended(*st, name) {
		return false
	}
	rigstate.SetSuspended(st, name, false)
	return true
}

func isRigSuspendedInState(st rigstate.SuspensionState, name string) bool {
	return rigstate.IsSuspended(st, name)
}

func buildMergedSuspendedRigNames(cfg *config.City, rs rigstate.SuspensionState) map[string]bool {
	names := rigstate.SuspendedNames(rs)
	for _, r := range cfg.Rigs {
		if r.Suspended {
			if names == nil {
				names = make(map[string]bool)
			}
			names[r.Name] = true
		}
	}
	return names
}
