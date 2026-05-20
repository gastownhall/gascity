// Package citystate manages per-city runtime state that is local to
// this machine and should not be committed to version control.
//
// State is stored in .gc/runtime/city-state.json. This is the symmetric
// counterpart to [internal/rigstate]: it keeps the explicit
// suspended/resumed preference for the city out of city.toml so each
// clone can have its own operational profile without merge conflicts.
//
// Suspension state is tri-state via [Override.Suspended]:
//
//   - nil    → no explicit preference; the effective state defers to
//     [config.Workspace.SuspendedOnStart] (committed default).
//   - &true  → explicit suspend (sticks across city restarts even if
//     SuspendedOnStart is false).
//   - &false → explicit resume (sticks across city restarts even if
//     SuspendedOnStart is true).
//
// Use [EffectiveSuspended] at every read site to compute the final
// suspension state by merging the runtime override with the workspace's
// SuspendedOnStart default.
package citystate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Override holds the city-level runtime overrides. New fields can be
// added here as the system grows without changing the file format.
//
// Suspended is a tri-state pointer mirroring rigstate.RigOverride.Suspended.
type Override struct {
	Suspended *bool `json:"suspended,omitempty"`
}

// State is the runtime city state persisted to disk. The wrapping
// struct is intentional so the file format can grow with sibling
// override blocks (e.g. provider readiness pins, idle defaults).
type State struct {
	City      Override  `json:"city"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Load reads the runtime city state. Returns an empty state (not an
// error) if the file does not exist.
func Load(fs fsys.FS, cityPath string) (State, error) {
	p := citylayout.CityStateFile(cityPath)
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

// Save writes the runtime city state to disk atomically.
func Save(fs fsys.FS, cityPath string, st State) error {
	p := citylayout.CityStateFile(cityPath)
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

// IsSuspended reports whether the city is explicitly suspended in the
// runtime state. An explicit resume (&false) and the no-preference
// state (nil) both return false. Callers that want the effective
// state including SuspendedOnStart should use [EffectiveSuspended].
func IsSuspended(st State) bool {
	return st.City.Suspended != nil && *st.City.Suspended
}

// ExplicitSuspended returns the explicit city suspension preference
// recorded in runtime state, if any. The second return value is true
// iff an explicit preference exists (Suspended is non-nil).
func ExplicitSuspended(st State) (suspended, ok bool) {
	if st.City.Suspended == nil {
		return false, false
	}
	return *st.City.Suspended, true
}

// EffectiveSuspended computes the effective city suspension state by
// merging the runtime override with the workspace's SuspendedOnStart
// default. The runtime override wins when present.
func EffectiveSuspended(st State, suspendedOnStart bool) bool {
	if v, ok := ExplicitSuspended(st); ok {
		return v
	}
	return suspendedOnStart
}

// SetCitySuspended records an explicit city suspension preference.
// Pass nil to clear (defer to SuspendedOnStart), &true / &false to
// record explicit suspend / resume. Returns without rewriting the file
// when on-disk state already matches the requested value (preserves
// UpdatedAt + mtime).
func SetCitySuspended(fs fsys.FS, cityPath string, suspended *bool) error {
	st, err := Load(fs, cityPath)
	if err != nil {
		return err
	}
	before, hadBefore := ExplicitSuspended(st)
	switch {
	case suspended == nil && !hadBefore:
		return nil
	case suspended != nil && hadBefore && before == *suspended:
		return nil
	}
	st.City.Suspended = suspended
	return Save(fs, cityPath, st)
}
