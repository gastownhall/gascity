package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

// RigPackError is a failure to compose one rig's packs: resolving or
// validating an include or import, loading it, or applying the rig's pack
// requirements, runtimes or overrides. Its message is the wrapped error's,
// so callers that print it see the same text as before it was typed.
type RigPackError struct {
	// Rig is the name of the rig whose packs failed to compose.
	Rig string
	// Err is the underlying composition error.
	Err error
}

// Error returns the underlying error's message.
func (e *RigPackError) Error() string { return e.Err.Error() }

// Unwrap returns the underlying error.
func (e *RigPackError) Unwrap() error { return e.Err }

// RigQuarantine records a rig whose pack content an isolating load withheld.
type RigQuarantine struct {
	// Rig is the quarantined rig's name.
	Rig string
	// Err is the error a strict load would have failed with.
	Err error
}

// rigQuarantineWarningMarker identifies the one LoadWarnings entry a
// quarantined rig produces.
const rigQuarantineWarningMarker = " quarantined: its packs are not loaded: "

// IsRigQuarantineWarning reports whether a load warning announces a
// quarantined rig.
func IsRigQuarantineWarning(warning string) bool {
	return strings.HasPrefix(warning, "rig ") && strings.Contains(warning, rigQuarantineWarningMarker)
}

// RigQuarantined reports whether the named rig's packs were withheld by an
// isolating load.
func (c *City) RigQuarantined(name string) bool {
	if c == nil {
		return false
	}
	for _, q := range c.QuarantinedRigs {
		if q.Rig == name {
			return true
		}
	}
	return false
}

// QuarantineError returns the errors a strict load would have reported for
// the quarantined rigs, joined, or nil when no rig was quarantined. Callers
// that validate config use it to turn a quarantine back into a failure.
func (c *City) QuarantineError() error {
	if c == nil || len(c.QuarantinedRigs) == 0 {
		return nil
	}
	errs := make([]error, 0, len(c.QuarantinedRigs))
	for _, q := range c.QuarantinedRigs {
		errs = append(errs, q.Err)
	}
	return fmt.Errorf("expanding packs: %w", errors.Join(errs...))
}

// rigQuarantineState carries the rigs an isolating load excludes through one
// composition attempt, and collects the city patches withheld because they
// target an excluded rig.
type rigQuarantineState struct {
	rigs     []RigQuarantine
	excluded map[string]bool
	withheld map[string][]string
}

func (q *rigQuarantineState) excludedRigs() map[string]bool {
	if q == nil {
		return nil
	}
	return q.excluded
}

// withholdPatch reports whether a city [[patches.agent]] entry targets a
// quarantined rig and matches no agent there, recording it for the rig's
// warning. Such a patch cannot apply while the rig's packs are withheld; a
// patch aimed at a healthy rig is never withheld, so typos still fail.
func (q *rigQuarantineState) withholdPatch(root *City, p *AgentPatch) bool {
	if q == nil || len(q.excluded) == 0 {
		return false
	}
	dir, err := agentPatchTargetDir(p)
	if err != nil || !q.excluded[dir] || agentPatchMatchesExisting(root, p) {
		return false
	}
	q.withheld[dir] = append(q.withheld[dir], dir+"/"+p.Name)
	return true
}

func (q *rigQuarantineState) reset() {
	q.withheld = make(map[string][]string)
}

func (q *rigQuarantineState) warning(r RigQuarantine) string {
	w := fmt.Sprintf("rig %q%s%v", r.Rig, rigQuarantineWarningMarker, r.Err)
	if patches := q.withheld[r.Rig]; len(patches) > 0 {
		w += fmt.Sprintf(" (withheld [[patches.agent]]: %s)", strings.Join(patches, ", "))
	}
	return w
}

// loadIsolatingRigPackFailures composes the city, quarantining each rig whose
// packs fail to compose. A failing rig is excluded and the city recomposed
// from the top, so a quarantined rig leaves nothing half-merged behind; each
// retry excludes one more rig, which bounds the loop by the rig count.
func loadIsolatingRigPackFailures(fs fsys.FS, path string, opts LoadOptions, extraIncludes []string) (*City, *Provenance, error) {
	q := &rigQuarantineState{excluded: make(map[string]bool)}
	for {
		q.reset()
		cfg, prov, err := loadWithIncludes(fs, path, opts, q, extraIncludes...)
		if err == nil {
			cfg.QuarantinedRigs = append([]RigQuarantine(nil), q.rigs...)
			for _, r := range q.rigs {
				w := q.warning(r)
				cfg.LoadWarnings = appendUnique(cfg.LoadWarnings, w)
				prov.Warnings = appendUnique(prov.Warnings, w)
			}
			return cfg, prov, nil
		}
		var rigErr *RigPackError
		if !errors.As(err, &rigErr) || errors.Is(err, ErrRepoCacheBusy) || q.excluded[rigErr.Rig] {
			return nil, nil, err
		}
		q.excluded[rigErr.Rig] = true
		q.rigs = append(q.rigs, RigQuarantine{Rig: rigErr.Rig, Err: rigErr})
	}
}
