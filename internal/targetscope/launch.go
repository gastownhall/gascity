// Launch phase: declare every member, then stamp the root.
//
// The design (§5) requires scope stamping at EVERY root-creation boundary and
// warns against silent holes. The defence against a hole is not diligence at
// seven call sites — it is that all seven call ONE function, so a boundary can
// only be wrong by not calling it at all, never by calling it differently.
//
// PHASE ORDER IS THE INVARIANT. Every member declaration commits before the
// root is materialized. That ordering is what makes a rejected launch safe:
// the declarations that did commit are immutable-correct and reusable by a
// retry, and because no root exists yet, no claimable graph work is orphaned.
// Stamping the root before declaring members would invert this and leave a
// live root pointing at members that then refuse to declare.
package targetscope

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/formula"
)

// StampMetadata writes scope into a metadata map, allocating one when meta is
// nil, and returns the map for the caller to assign back.
//
// It is the only writer of the metadata key outside the member-declaration
// protocol, and it is deliberately confined to values that have not been
// materialized yet: recipe steps before instantiation. A bead that already
// exists is declared, never stamped, because only the declaration protocol
// carries the single-winner exclusion.
func StampMetadata(meta map[string]string, scope Scope) (map[string]string, error) {
	blob, err := Marshal(scope)
	if err != nil {
		return meta, err
	}
	if meta == nil {
		meta = make(map[string]string, 1)
	}
	meta[beadmeta.TargetScopeMetadataKey] = blob
	return meta, nil
}

// StampRoot writes the resolved scope onto a recipe's root step.
//
// Only the root is stamped. Generated stages inherit through the root chain
// that ResolveInherited walks, so stamping each stage would duplicate a fact
// that already has one home and invite the two copies to disagree.
//
// An unknown scope is stamped, not skipped: §2c requires a boundary to leave a
// present-valid field-empty object rather than nothing at all. Absence is what
// lets a later claim or reconcile write cwd; a field-empty object is what
// makes the readers report unknown and refuse the flat fallback.
func StampRoot(recipe *formula.Recipe, scope Scope) error {
	if recipe == nil || len(recipe.Steps) == 0 {
		return fmt.Errorf("targetscope: cannot stamp a recipe with no steps")
	}
	root := &recipe.Steps[0]
	meta, err := StampMetadata(root.Metadata, scope)
	if err != nil {
		return fmt.Errorf("stamping target scope on recipe root: %w", err)
	}
	root.Metadata = meta
	return nil
}

// PrepareLaunch runs the pre-materialization phase for one launch: declare
// every resolved member under its single-winner exclusion, then stamp the
// root.
//
// The caller must treat any error as a LAUNCH REJECTION and materialize
// nothing. That is the whole point of running before materialization: a
// rejection here costs a launch, whereas the same rejection after the root
// exists costs an orphaned graph.
func PrepareLaunch(recipe *formula.Recipe, scope Scope, members []Member) error {
	if err := DeclareAll(members); err != nil {
		return err
	}
	return StampRoot(recipe, scope)
}

// FormulaDefaultVars extracts the [vars.*].default layer as a plain map.
//
// This layer is a trusted source (§3a, lowest precedence) and including it is
// what keeps the scope object and the consumed carrier identical in the
// default-only case: with no branch supplied anywhere, scope.branch and the
// carrier the formula substitutes must both be the formula's own default,
// rather than the scope being unknown while substitution quietly uses it.
func FormulaDefaultVars(f *formula.Formula) map[string]string {
	if f == nil || len(f.Vars) == 0 {
		return nil
	}
	defaults := make(map[string]string, len(f.Vars))
	for name, def := range f.Vars {
		if def == nil || def.Default == nil {
			continue
		}
		defaults[name] = *def.Default
	}
	if len(defaults) == 0 {
		return nil
	}
	return defaults
}
