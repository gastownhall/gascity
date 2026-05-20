// Package suspensionstate manages the city-local runtime suspension
// state file (.gc/runtime/suspension-state.json). It is the single
// source of truth for **live** city, rig, and (eventually) agent
// suspension preferences. Authored startup defaults live in
// city.toml's suspended_on_start fields; this package holds only the
// transient runtime overrides recorded by `gc suspend` / `gc resume`
// and their rig/agent counterparts.
//
// The file is intentionally per-clone and gitignored: it never leaks
// into committed config, so each developer's personal "mute this
// rig" choice does not collide on merge.
//
// # Schema
//
// One JSON document with three sibling override blocks:
//
//	{
//	  "city":   { "suspended": true },
//	  "rigs":   { "alpha": { "suspended": false } },
//	  "agents": { "rig/worker": { "suspended": true } },
//	  "updated_at": "2026-05-20T..."
//	}
//
// Only the `city` and `rigs` blocks are wired today. `agents` is a
// reserved schema slot consumed by the follow-up tracked in issue
// #2407; this package will gain symmetric agent helpers there.
//
// # Tri-state Override
//
// Each Override.Suspended is a *bool with three meanings:
//
//   - nil    → no explicit preference; the effective state defers to
//     the corresponding `suspended_on_start` in city.toml.
//   - &true  → explicit suspend (sticks across city restarts even if
//     suspended_on_start is false).
//   - &false → explicit resume (sticks across city restarts even if
//     suspended_on_start is true).
//
// Always use [EffectiveCitySuspended] / [EffectiveRigSuspended] at
// read sites to compute the merged result of runtime override and
// authored startup default.
package suspensionstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Override holds one scope's runtime overrides. New fields can be
// added without changing the file format.
//
// Suspended is the tri-state preference described in the package
// overview. New fields (e.g. model pin, idle timeout override) can be
// appended later without breaking forward compatibility because the
// JSON decoder ignores unknown fields and `omitempty` drops zero
// values on write.
type Override struct {
	Suspended *bool `json:"suspended,omitempty"`
}

// State is the runtime suspension state persisted to disk.
type State struct {
	// City is the city-level override.
	City Override `json:"city"`
	// Rigs is keyed by rig name (matches config.Rig.Name).
	Rigs map[string]Override `json:"rigs,omitempty"`
	// Agents is reserved for the agent-suspension follow-up
	// (issue #2407). Keyed by qualified agent name. Decoded so a
	// future binary can write the field without breaking older
	// readers; not consumed by any code path today.
	Agents map[string]Override `json:"agents,omitempty"`
	// UpdatedAt is stamped by Save on every disk write.
	UpdatedAt time.Time `json:"updated_at"`
}

// Load reads the runtime suspension state. Returns a zero-value State
// (not an error) when the file does not yet exist.
func Load(fs fsys.FS, cityPath string) (State, error) {
	p := citylayout.SuspensionStateFile(cityPath)
	data, err := fs.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Save writes the runtime suspension state to disk atomically.
func Save(fs fsys.FS, cityPath string, st State) error {
	p := citylayout.SuspensionStateFile(cityPath)
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

// ----- City helpers -----

// IsCitySuspended reports whether the city is explicitly suspended in
// the runtime state. An explicit resume (&false) and the
// no-preference state (nil) both return false. Callers that want the
// effective state including the authored startup default should use
// [EffectiveCitySuspended].
func IsCitySuspended(st State) bool {
	return st.City.Suspended != nil && *st.City.Suspended
}

// ExplicitCity returns the explicit city suspension preference
// recorded in runtime state, if any. The second return value is true
// iff an explicit preference exists (Suspended is non-nil).
func ExplicitCity(st State) (suspended, ok bool) {
	if st.City.Suspended == nil {
		return false, false
	}
	return *st.City.Suspended, true
}

// EffectiveCitySuspended computes the effective city suspension state
// by merging the runtime override with the workspace's
// SuspendedOnStart default. The runtime override wins when present.
func EffectiveCitySuspended(st State, suspendedOnStart bool) bool {
	if v, ok := ExplicitCity(st); ok {
		return v
	}
	return suspendedOnStart
}

// SetCity records an explicit city suspension preference on st. Pass
// nil to clear the preference (effective state will then defer to
// the workspace's SuspendedOnStart); pass &true / &false to record
// the explicit user choice.
func SetCity(st *State, suspended *bool) {
	st.City.Suspended = suspended
}

// SetCitySuspended is a convenience that loads state, records an
// explicit city suspension preference, and saves. Returns without
// rewriting the file when on-disk state already matches the
// requested value (preserves UpdatedAt + mtime).
func SetCitySuspended(fs fsys.FS, cityPath string, suspended *bool) error {
	st, err := Load(fs, cityPath)
	if err != nil {
		return err
	}
	before, had := ExplicitCity(st)
	switch {
	case suspended == nil && !had:
		return nil
	case suspended != nil && had && before == *suspended:
		return nil
	}
	SetCity(&st, suspended)
	return Save(fs, cityPath, st)
}

// ----- Rig helpers -----

// IsRigSuspended reports whether the given rig is explicitly
// suspended in the runtime state (Suspended is &true). An explicit
// resume and the no-preference state both return false. Callers that
// want the effective merged state should use [EffectiveRigSuspended].
func IsRigSuspended(st State, name string) bool {
	r, ok := st.Rigs[name]
	return ok && r.Suspended != nil && *r.Suspended
}

// ExplicitRig returns the explicit suspension preference for a rig
// recorded in runtime state, if any. The second return value is true
// iff an explicit preference exists.
func ExplicitRig(st State, name string) (suspended, ok bool) {
	r, present := st.Rigs[name]
	if !present || r.Suspended == nil {
		return false, false
	}
	return *r.Suspended, true
}

// EffectiveRigSuspended computes the effective suspension state for
// a rig by merging the runtime override with the rig's
// SuspendedOnStart default. The runtime override wins when present.
func EffectiveRigSuspended(st State, name string, suspendedOnStart bool) bool {
	if v, ok := ExplicitRig(st, name); ok {
		return v
	}
	return suspendedOnStart
}

// SetRig records an explicit rig suspension preference on st. Pass
// nil to clear (effective state defers to SuspendedOnStart); pass
// &true / &false to record the explicit user choice. The entry is
// removed when no overrides remain so the JSON file stays minimal.
func SetRig(st *State, name string, suspended *bool) {
	r := st.Rigs[name]
	r.Suspended = suspended
	if r == (Override{}) {
		delete(st.Rigs, name)
		return
	}
	if st.Rigs == nil {
		st.Rigs = make(map[string]Override)
	}
	st.Rigs[name] = r
}

// SetRigSuspended is a convenience that loads state, records an
// explicit rig suspension preference, and saves. Returns without
// rewriting the file when on-disk state already matches the
// requested value.
func SetRigSuspended(fs fsys.FS, cityPath, name string, suspended *bool) error {
	st, err := Load(fs, cityPath)
	if err != nil {
		return err
	}
	before, had := ExplicitRig(st, name)
	switch {
	case suspended == nil && !had:
		return nil
	case suspended != nil && had && before == *suspended:
		return nil
	}
	SetRig(&st, name, suspended)
	return Save(fs, cityPath, st)
}

// SuspendedRigNames returns the set of rig names whose runtime state
// records an explicit suspend (&true). Rigs with an explicit resume
// (&false) or no entry are not included. Callers that want the
// effective merged-with-config set should iterate the config and
// call [EffectiveRigSuspended] for each.
func SuspendedRigNames(st State) map[string]bool {
	names := make(map[string]bool)
	for name, r := range st.Rigs {
		if r.Suspended != nil && *r.Suspended {
			names[name] = true
		}
	}
	return names
}
