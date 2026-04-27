// Package rigstate manages per-rig runtime state that is local to this
// machine and should not be committed to version control.
//
// State is stored in .gc/runtime/rig-state.json. This keeps per-user
// preferences (which rigs are suspended, future: model overrides, idle
// timeout overrides) out of city.toml so each clone can have its own
// operational profile without merge conflicts.
//
// For backwards compatibility, callers should also check config.Rig
// fields (e.g. Suspended) and merge both sources.
package rigstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// RigOverride holds per-rig runtime overrides. Only non-zero fields
// are considered active. New fields can be added here as the system
// grows (e.g. model, idle timeout, max sessions).
type RigOverride struct {
	Suspended bool `json:"suspended,omitempty"`
}

// SuspensionState is the runtime rig state persisted to disk.
// This struct — and the rig-state.json file it lives in — is designed to
// grow: future per-rig runtime overrides (model, idle timeout, max sessions)
// can be added to RigOverride without changing the file format.
type SuspensionState struct {
	Rigs      map[string]RigOverride `json:"rigs"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// Load reads the runtime rig state. Returns an empty state (not an
// error) if the file does not exist.
func Load(fs fsys.FS, cityPath string) (SuspensionState, error) {
	p := citylayout.RigStateFile(cityPath)
	data, err := fs.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SuspensionState{Rigs: make(map[string]RigOverride)}, nil
		}
		return SuspensionState{}, err
	}
	var st SuspensionState
	if err := json.Unmarshal(data, &st); err != nil {
		return SuspensionState{}, err
	}
	if st.Rigs == nil {
		st.Rigs = make(map[string]RigOverride)
	}
	return st, nil
}

// Save writes the runtime rig state to disk atomically.
func Save(fs fsys.FS, cityPath string, st SuspensionState) error {
	p := citylayout.RigStateFile(cityPath)
	if err := fs.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsys.WriteFileAtomic(fs, p, data, 0o644)
}

// IsSuspended reports whether the given rig is suspended in the
// runtime state.
func IsSuspended(st SuspensionState, name string) bool {
	r, ok := st.Rigs[name]
	return ok && r.Suspended
}

// SetSuspended sets or clears the suspended flag for a rig. Removes
// the rig entry entirely when all overrides return to zero values.
func SetSuspended(st *SuspensionState, name string, suspended bool) {
	r := st.Rigs[name]
	r.Suspended = suspended
	if r == (RigOverride{}) {
		delete(st.Rigs, name)
	} else {
		st.Rigs[name] = r
	}
}

// SetRigSuspended is a convenience that loads state, sets or clears a
// rig's suspension, and saves.
func SetRigSuspended(fs fsys.FS, cityPath, name string, suspended bool) error {
	st, err := Load(fs, cityPath)
	if err != nil {
		return err
	}
	before := IsSuspended(st, name)
	if before == suspended {
		return nil
	}
	SetSuspended(&st, name, suspended)
	return Save(fs, cityPath, st)
}

// SuspendedNames returns the set of rig names that are suspended in the
// runtime state.
func SuspendedNames(st SuspensionState) map[string]bool {
	names := make(map[string]bool)
	for name, r := range st.Rigs {
		if r.Suspended {
			names[name] = true
		}
	}
	return names
}
