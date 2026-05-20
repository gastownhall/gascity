// Package rigstate manages per-rig runtime state that is local to this
// machine and should not be committed to version control.
//
// State is stored in .gc/runtime/rig-state.json. This keeps per-user
// preferences (which rigs are suspended, future: model overrides, idle
// timeout overrides) out of city.toml so each clone can have its own
// operational profile without merge conflicts.
//
// Suspension state is tri-state via [RigOverride.Suspended]:
//
//   - nil    → no explicit preference; the effective state defers to
//     [config.Rig.SuspendedOnStart] (committed default).
//   - &true  → explicit suspend (sticks across city restarts even if
//     SuspendedOnStart is false).
//   - &false → explicit resume (sticks across city restarts even if
//     SuspendedOnStart is true).
//
// Use [EffectiveSuspended] at every read site to compute the final
// suspension state by merging the runtime override with the rig's
// SuspendedOnStart default.
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

// RigOverride holds per-rig runtime overrides. Only non-zero fields are
// considered active. New fields can be added here as the system grows
// (e.g. model, idle timeout, max sessions).
//
// Suspended is a tri-state pointer: nil means "no preference recorded"
// (the rig's [config.Rig.SuspendedOnStart] applies); a non-nil pointer
// records an explicit user choice that overrides SuspendedOnStart and
// sticks across city restarts.
type RigOverride struct {
	Suspended *bool `json:"suspended,omitempty"`
}

// SuspensionState is the runtime rig state persisted to disk. This
// struct — and the rig-state.json file it lives in — is designed to
// grow: future per-rig runtime overrides (model, idle timeout, max
// sessions) can be added to RigOverride without changing the file
// format.
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

// IsSuspended reports whether the given rig is explicitly suspended in
// the runtime state (i.e., Suspended is a non-nil &true). An explicit
// resume (&false) and the no-preference state (nil) both return false.
// Callers that want the effective state including SuspendedOnStart
// should use [EffectiveSuspended].
func IsSuspended(st SuspensionState, name string) bool {
	r, ok := st.Rigs[name]
	return ok && r.Suspended != nil && *r.Suspended
}

// ExplicitSuspended returns the explicit suspension preference for a
// rig recorded in runtime state, if any. The second return value is
// true iff an explicit preference exists (Suspended is non-nil).
func ExplicitSuspended(st SuspensionState, name string) (suspended, ok bool) {
	r, present := st.Rigs[name]
	if !present || r.Suspended == nil {
		return false, false
	}
	return *r.Suspended, true
}

// EffectiveSuspended computes the effective suspension state for a rig
// by merging the runtime override with the rig's SuspendedOnStart
// default. The runtime override wins when present.
func EffectiveSuspended(st SuspensionState, name string, suspendedOnStart bool) bool {
	if v, ok := ExplicitSuspended(st, name); ok {
		return v
	}
	return suspendedOnStart
}

// SetSuspended records an explicit suspension preference for a rig.
// Pass nil to clear the preference (effective state will then defer to
// [config.Rig.SuspendedOnStart]); pass &true / &false to record the
// explicit user choice. The rig entry is removed entirely when no
// overrides remain so the JSON file stays minimal.
func SetSuspended(st *SuspensionState, name string, suspended *bool) {
	r := st.Rigs[name]
	r.Suspended = suspended
	if r == (RigOverride{}) {
		delete(st.Rigs, name)
		return
	}
	if st.Rigs == nil {
		st.Rigs = make(map[string]RigOverride)
	}
	st.Rigs[name] = r
}

// SetRigSuspended is a convenience that loads state, records an
// explicit suspension preference, and saves. Pass nil to clear the
// preference, &true for explicit suspend, &false for explicit resume.
// Returns without rewriting the file when on-disk state already
// matches the requested value (preserves UpdatedAt + mtime).
func SetRigSuspended(fs fsys.FS, cityPath, name string, suspended *bool) error {
	st, err := Load(fs, cityPath)
	if err != nil {
		return err
	}
	before, hadBefore := ExplicitSuspended(st, name)
	switch {
	case suspended == nil && !hadBefore:
		return nil
	case suspended != nil && hadBefore && before == *suspended:
		return nil
	}
	SetSuspended(&st, name, suspended)
	return Save(fs, cityPath, st)
}

// SuspendedNames returns the set of rig names that are explicitly
// suspended in the runtime state (Suspended == &true). Rigs with an
// explicit resume (&false) or no entry are not included. Callers that
// want the effective merged-with-config set should also iterate the
// config's Rigs and call [EffectiveSuspended] for each.
func SuspendedNames(st SuspensionState) map[string]bool {
	names := make(map[string]bool)
	for name, r := range st.Rigs {
		if r.Suspended != nil && *r.Suspended {
			names[name] = true
		}
	}
	return names
}
