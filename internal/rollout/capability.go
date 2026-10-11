package rollout

import (
	"context"

	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// Capability reports whether the runtime can execute a gate's new path; the
// definition lives in the dependency-leaf gate package so store-layer
// consumers can supply predicates without importing rollout (which would
// cycle via config → orders → beads). See gate.Capability for the contract.
type Capability = gate.Capability

// Decision is the four-way verdict of the enable-AND-capable product. See
// gate.Decision.
type Decision = gate.Decision

const (
	// UseLegacy runs the old path (Off, or ModeUnset defaulted to Off).
	UseLegacy = gate.UseLegacy
	// UseNew runs the new path (Auto or Require, and capable).
	UseNew = gate.UseNew
	// DegradeLoud runs the old path with an obligatory diagnostic.
	DegradeLoud = gate.DegradeLoud
	// RefuseClosed is a typed refusal that must not fall back to the old path.
	RefuseClosed = gate.RefuseClosed
)

// ResolveCapability computes the enable-AND-capable product; see
// gate.ResolveCapability for the full cell contract.
func ResolveCapability(ctx context.Context, mode Mode, pred Capability) (Decision, string) {
	return gate.ResolveCapability(ctx, mode, pred)
}
