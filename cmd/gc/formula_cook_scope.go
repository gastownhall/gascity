package main

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// cookScopeSources builds the trusted-source set for a `gc formula cook`.
//
// SCOPE RESOLVES FROM cookVars' OWN INPUTS, PER THE D4 RULE. Cook substitutes
// cookVars — graphv2 EffectiveRuntimeVars, i.e. the formula defaults overlaid by
// the caller's --var — into the recipe, so the scope must resolve from EXACTLY
// those inputs and no higher layer: the caller's explicit --var (precedence 1,
// kept UNFLATTENED so a duplicated branch carrier is a loud error rather than a
// silent last-write-wins) and the formula defaults (lowest precedence).
//
// Cook injects NO rig or routing var into its own substitution:
// EffectiveRuntimeVars folds in only defaults + caller vars, and
// decorateFormulaCookGraphV2Recipe runs AFTER compile and rewrites recipe
// ROUTING, never cookVars. Resolving the scope from the rig would therefore
// stamp a branch the cooked work never runs against — manufacturing the exact
// scope != consumed divergence the equality invariant (§3c, E2E #16) forbids.
// This is the order boundary's measured constraint (orderScopeSources) applied
// to cook, which substitutes cookVars where an order substitutes nothing.
func cookScopeSources(rawVars []string, f *formula.Formula) targetscope.Sources {
	return targetscope.Sources{
		Explicit:        rawVars,
		FormulaDefaults: targetscope.FormulaDefaultVars(f),
	}
}

// resolveCookLaunchScope resolves a cook launch's target scope, writes the
// reconciled branch back into cookVars so template substitution and the scope
// object agree, and returns the members that must declare the scope before the
// root is materialized.
//
// CALL THIS BEFORE COMPILING. The reconciled branch has to be in cookVars
// before substitution runs, or the formula substitutes one branch while the
// scope records another. An error is a LAUNCH REJECTION — nothing is
// materialized at this point, which is why the phase runs here.
//
// The member layer (§3a precedence 2) is filled from any scope already
// governing the attach target, so a cook onto an already-scoped bead INHERITS
// that scope instead of resolving a lower-layer default and colliding with it.
// A genuine retarget still rejects: an explicit branch that overrides the member
// is caught here, and a member the resolver never saw (a convoy sibling) is
// caught by its own CAS declaration.
func resolveCookLaunchScope(store beads.Store, f *formula.Formula, rawVars []string, cookVars map[string]string, attachID, convoyID, scopeRoot string) (targetscope.Scope, []targetscope.Member, error) {
	sources := cookScopeSources(rawVars, f)
	if err := applyCookTargetMemberLayer(&sources, store, attachID); err != nil {
		return targetscope.Scope{}, nil, err
	}

	resolved, err := targetscope.Resolve(sources, scopeRoot)
	if err != nil {
		return targetscope.Scope{}, nil, fmt.Errorf("resolving target scope: %w", err)
	}

	// Immutability at the attach target (§5b). The resolver lets an explicit
	// branch (precedence 1) outrank an existing member scope silently; for a
	// convoy member that is caught by the CAS declaration below, but a target
	// read into the member layer here must be rejected explicitly before
	// materialization when the resolved scope differs from what it already
	// carries — a retarget is a launch rejection, not a rewrite.
	if sources.MemberSet && !sources.Member.Equal(resolved.Scope) {
		return targetscope.Scope{}, nil, fmt.Errorf("%w: %s is already scoped to %+v; refusing to retarget it to %+v", targetscope.ErrInvalidScope, attachID, sources.Member, resolved.Scope)
	}

	// The consumed-carrier write-back. Which carrier the formula reads is
	// derived from the RESOLVED formula's declared vars, never the formula-name
	// predicates: a formula that declares base_branch without matching a name
	// pattern still consumes it.
	targetscope.ApplyToVars(cookVars, resolved, targetscope.DeclaresVar(f, targetscope.VarBaseBranch), targetscope.DeclaresVar(f, targetscope.VarCarrierTargetBranch))

	members, err := cookScopeMembers(store, convoyID, resolved.Scope)
	if err != nil {
		return targetscope.Scope{}, nil, err
	}
	return resolved.Scope, members, nil
}

// applyCookTargetMemberLayer fills the member layer from the scope already
// governing the bead a cook --attach runs ON, read through the inheritance walk
// so a stage's symbolic root reference resolves to the governing scope rather
// than a bead-local miss.
//
// A present-invalid scope is a LAUNCH REJECTION, never a fall-through: "I could
// not read the scope governing this bead" must not become permission to choose a
// new one. A store miss is NOT treated as a member signal — the attach bead may
// live in a validation-only querier, and the attach itself surfaces a real error
// downstream if it is genuinely absent.
func applyCookTargetMemberLayer(sources *targetscope.Sources, store beads.Store, attachID string) error {
	if attachID == "" || store == nil {
		return nil
	}
	bead, err := store.Get(attachID)
	if err != nil {
		return nil
	}
	res := targetscope.ResolveInherited(store, bead)
	switch res.State {
	case targetscope.StateValid:
		sources.Member = res.Scope
		sources.MemberSet = true
	case targetscope.StateInvalid:
		return fmt.Errorf("target scope of %s is unusable: %w", attachID, res.Reason)
	}
	return nil
}

// cookScopeMembers lists the tracked members of a cook's input convoy that must
// declare this scope under CAS before the root is materialized.
//
// A graph cook --attach normalizes its target into a single-item input convoy in
// THIS SAME store (CreateSingleItemInputConvoy), so the target and every other
// convoy member are present in the declaration store — cook has none of the
// validation-only-querier split that forces the sling legacy-attach source onto
// a non-fatal stamp. Closed members are included: a member closed between convoy
// creation and cook is still a bead whose close gate can run.
// prepareCookLaunch runs the pre-materialization phase for a cook: declare every
// member under its single-winner exclusion, then stamp the recipe root. It wraps
// targetscope.PrepareLaunch so the cook call sites need no direct targetscope
// import, and so a boundary can be wrong by not calling it, never by calling it
// differently.
func prepareCookLaunch(recipe *formula.Recipe, scope targetscope.Scope, members []targetscope.Member) error {
	return targetscope.PrepareLaunch(recipe, scope, members)
}

func cookScopeMembers(store beads.Store, convoyID string, scope targetscope.Scope) ([]targetscope.Member, error) {
	if store == nil || strings.TrimSpace(convoyID) == "" {
		return nil, nil
	}
	beadsInConvoy, err := convoycore.Members(store, convoyID, true)
	if err != nil {
		return nil, fmt.Errorf("listing members of input convoy %s: %w", convoyID, err)
	}
	members := make([]targetscope.Member, 0, len(beadsInConvoy))
	for _, b := range beadsInConvoy {
		members = append(members, targetscope.Member{ID: b.ID, Store: store, Scope: scope})
	}
	return members, nil
}
