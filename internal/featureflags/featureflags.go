// Package featureflags centralizes the process-global feature-flag state that
// the formula compiler and molecule instantiator consult. Both flags derive
// from a single config source ([daemon] formula_v2) and move in lockstep, so
// this package is the one place that derivation and the global writes live:
// the CLI (applyFeatureFlags) and the API server (syncFeatureFlags) delegate
// here and cannot drift, and the CLI-unification characterization harness can
// bracket a lane with WithScoped so that constructing a server — which stomps
// the globals from its own city config — cannot contaminate another lane's
// captured output.
package featureflags

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

// Flags is a snapshot of the feature-flag state. FormulaV2 gates the formula
// compiler v2 capability; GraphApply gates molecule graph-apply batch
// instantiation. FromConfig always sets the two together, but Apply and
// Snapshot treat them independently so the harness can reason about each.
type Flags struct {
	FormulaV2  bool
	GraphApply bool
}

// FromConfig derives the flag state from a city config. A nil config yields
// the all-disabled state, matching the API server's historical nil-guard; a
// non-nil config with an absent [daemon] formula_v2 is enabled by default
// (see config.DaemonConfig.FormulaV2Enabled).
func FromConfig(cfg *config.City) Flags {
	enabled := cfg != nil && cfg.Daemon.FormulaV2Enabled()
	return Flags{FormulaV2: enabled, GraphApply: enabled}
}

// Snapshot reads the current process-global flag state.
func Snapshot() Flags {
	return Flags{
		FormulaV2:  formula.IsFormulaV2Enabled(),
		GraphApply: molecule.IsGraphApplyEnabled(),
	}
}

// Apply writes f to the process-global flag state consulted by the formula
// compiler and molecule instantiator. Safe for concurrent use with the
// Is*Enabled readers.
func Apply(f Flags) {
	formula.SetFormulaV2Enabled(f.FormulaV2)
	molecule.SetGraphApplyEnabled(f.GraphApply)
}

// WithScoped applies f, runs fn, then restores the prior flag state. It lets
// tests and the characterization harness bracket a lane so that constructing a
// server (which stomps the globals from its city config) cannot leak flag
// state into a sibling lane's capture.
//
// Two disciplines the harness must honor — WithScoped brackets, it does not
// enforce:
//   - It is NOT safe against concurrent lanes. Bracket flag-sensitive work
//     serially; the globals are process-wide.
//   - Constructing a server inside fn re-stomps the globals (api.New calls
//     syncFeatureFlags unconditionally), overwriting f for the rest of the
//     bracket. Pass f = FromConfig(laneCfg) so that interior stomp is
//     idempotent, or build the server outside the bracket. And because the
//     compiler/instantiator read the flags at use time (atomic.Bool.Load per
//     operation), a server still running after the bracket exits observes the
//     restored values — quiesce or tear each lane's server down before leaving.
func WithScoped(f Flags, fn func()) {
	prev := Snapshot()
	Apply(f)
	defer Apply(prev)
	fn()
}
