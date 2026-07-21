package sling

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// LaunchScope is what a launch boundary resolves before it materializes
// anything: the scope this launch runs under, and the members that must
// declare it first.
type LaunchScope struct {
	Scope   targetscope.Scope
	Members []targetscope.Member
	// BranchLayer records which trusted layer supplied the branch. Diagnostic
	// only — an operator asking "why did this run against release?" gets an
	// answer instead of a shrug.
	BranchLayer targetscope.LayerName
}

// ResolveLaunchScope resolves the target scope for one launch and writes the
// reconciled branch back into the variable map the formula will consume.
//
// CALL THIS BEFORE COMPILING. The winner has to be in vars before template
// substitution runs, or the formula substitutes one branch while the scope
// records another — the exact divergence §3c exists to close, and the
// equality E2E #16 asserts.
//
// An error is a LAUNCH REJECTION. Nothing has been materialized at this point,
// which is the whole reason the phase runs here: rejecting costs a launch,
// whereas the same rejection after the root exists costs an orphaned graph.
func ResolveLaunchScope(formulaName, beadID string, userVars []string, vars map[string]string, f *formula.Formula, convoyID string, a config.Agent, deps SlingDeps) (LaunchScope, error) {
	sources := SlingFormulaScopeSources(formulaName, beadID, userVars, a, deps)
	sources.FormulaDefaults = targetscope.FormulaDefaultVars(f)
	if err := applyTargetBeadMemberLayer(&sources, beadID, deps); err != nil {
		return LaunchScope{}, err
	}

	resolved, err := targetscope.Resolve(sources, SlingFormulaRepoDir(beadID, deps, a))
	if err != nil {
		return LaunchScope{}, fmt.Errorf("resolving target scope for %s: %w", formulaName, err)
	}

	// Immutability at the target bead (§5b). The resolver's cross-layer
	// precedence lets an explicit branch (precedence 1) outrank an existing
	// member scope (precedence 2) silently — the reconciler returns the explicit
	// winner without flagging that it overrode a governed bead. For the CONVOY
	// members below that is caught by the CAS declaration (present-valid-
	// different rejects), but the attach's claimable SOURCE is stamped, not
	// declared, so its immutability has to be enforced here, before anything is
	// materialized. A resolved scope that differs from the bead's already-valid
	// scope is a retarget, and a retarget is a launch rejection, not a rewrite.
	if sources.MemberSet && !sources.Member.Equal(resolved.Scope) {
		return LaunchScope{}, fmt.Errorf("%w: %s is already scoped to %+v; refusing to retarget it to %+v", targetscope.ErrInvalidScope, beadID, sources.Member, resolved.Scope)
	}

	// The consumed-carrier write-back. Which carrier the formula actually reads
	// is derived from the RESOLVED formula's declared vars, not from the
	// formula-name predicates: the predicates model what routing injects today
	// and are the right authority for that layer only. A formula that declares
	// base_branch without matching a name pattern still consumes base_branch.
	targetscope.ApplyToVars(vars, resolved, formulaDeclaresVar(f, targetscope.VarBaseBranch), formulaDeclaresVar(f, targetscope.VarCarrierTargetBranch))

	members, err := launchScopeMembers(convoyID, resolved.Scope, deps)
	if err != nil {
		return LaunchScope{}, err
	}
	return LaunchScope{Scope: resolved.Scope, Members: members, BranchLayer: resolved.BranchLayer}, nil
}

// applyTargetBeadMemberLayer fills the member layer (§3a precedence 2) from the
// scope already governing the bead a targeted launch runs ON.
//
// It reads through the inheritance walk rather than the bead's own key. A stage
// carries a symbolic root reference, not a copy of root metadata, so a
// bead-local read would report "no member scope" for precisely the beads whose
// scope is already decided — and the launch would then resolve a winner from a
// lower layer and pin a contradiction onto a bead that is already governed.
//
// A present-invalid scope is a LAUNCH REJECTION, never a fall-through to the
// lower layers: "I could not read the scope governing this bead" must never
// become permission to choose a new one.
func applyTargetBeadMemberLayer(sources *targetscope.Sources, beadID string, deps SlingDeps) error {
	if beadID == "" || deps.Store == nil {
		return nil
	}
	bead, err := deps.Store.Get(beadID)
	if err != nil {
		return fmt.Errorf("reading target scope of %s: %w", beadID, err)
	}
	res := targetscope.ResolveInherited(deps.Store, bead)
	switch res.State {
	case targetscope.StateValid:
		sources.Member = res.Scope
		sources.MemberSet = true
	case targetscope.StateInvalid:
		return fmt.Errorf("target scope of %s is unusable: %w", beadID, res.Reason)
	}
	return nil
}

// formulaDeclaresVar reports whether the resolved formula declares a variable,
// which is what makes it a carrier this formula actually consumes. It delegates
// so every launch boundary shares one answer.
func formulaDeclaresVar(f *formula.Formula, name string) bool {
	return targetscope.DeclaresVar(f, name)
}

// launchScopeMembers lists the tracked members that must declare this scope.
//
// Members are the pre-existing beads a targeted launch runs ON — the ones the
// close gate loads directly (§5b). They are what makes stamping the root
// insufficient on its own: a stage's scope is not the member's scope, and the
// gate reads the member.
//
// Closed members are included deliberately. A member closed between convoy
// creation and launch is still a bead whose gate can run, and skipping it
// would leave exactly the unscoped reader the design is closing.
//
// The claimable SOURCE bead of a --on/default attach is deliberately NOT a
// member here. A graph attach normalizes its target into this input convoy, so
// that bead already appears below; a legacy attach's source is not in any
// convoy and its scope is stamped at the molecule_id write instead
// (stampClaimableSourceScope). The distinction matters because members are
// declared under CAS, which fails closed when the source is absent from the
// declaration store — and a legacy attach's source can be in a validation-only
// querier, exactly the shape the molecule_id write already tolerates non-
// fatally.
func launchScopeMembers(convoyID string, scope targetscope.Scope, deps SlingDeps) ([]targetscope.Member, error) {
	if convoyID == "" || deps.Store == nil {
		return nil, nil
	}
	beadsInConvoy, err := convoycore.Members(deps.Store, convoyID, true)
	if err != nil {
		return nil, fmt.Errorf("listing members of input convoy %s: %w", convoyID, err)
	}
	members := make([]targetscope.Member, 0, len(beadsInConvoy))
	for _, b := range beadsInConvoy {
		members = append(members, targetscope.Member{ID: b.ID, Store: deps.Store, Scope: scope})
	}
	return members, nil
}

// stampClaimableSourceScope stamps the resolved scope on the pre-existing bead a
// legacy attach routes and closes — the SOURCE bead — co-located with the
// molecule_id write that makes it the claimable unit.
//
// It is a plain SetMetadata, non-fatal like the molecule_id write beside it,
// NOT a CAS member declaration. Two facts make that correct rather than a
// weakening. First, immutability is already enforced upstream: ResolveLaunchScope
// reads this bead's existing scope into the member layer and rejects the launch
// before any materialization if the resolved scope differs — so by the time we
// reach this stamp the scope either EQUALS what the bead already carries (an
// idempotent re-stamp) or the bead was unscoped (a fresh stamp); there is no
// path here that silently retargets a governed bead. Second, the exclusion the
// member CAS would add is already held by the source-workflow lock that
// serializes attaches on this bead, and the write must match molecule_id's
// non-fatal contract because a legacy source can live in a validation-only
// querier absent from the declaration store.
//
// An unknown scope is still stamped (§2c): absence is what re-enables the cwd
// writers, so "no branch to record" is a present-valid field-empty object, not
// nothing.
func stampClaimableSourceScope(deps SlingDeps, beadID string, scope targetscope.Scope, result *SlingResult) {
	if deps.Store == nil || beadID == "" {
		return
	}
	blob, err := targetscope.Marshal(scope)
	if err != nil {
		result.MetadataErrors = append(result.MetadataErrors, fmt.Sprintf("marshaling target scope for %s: %v", beadID, err))
		return
	}
	if err := deps.Store.SetMetadata(beadID, beadmeta.TargetScopeMetadataKey, blob); err != nil {
		result.MetadataErrors = append(result.MetadataErrors, fmt.Sprintf("stamping target scope on %s: %v", beadID, err))
	}
}

// PrepareLaunchScope runs the declaration phase and stamps the recipe root.
//
// It is the one call every sling launch boundary makes, so a boundary can be
// wrong by not calling it, never by calling it differently.
func (l LaunchScope) PrepareLaunchScope(recipe *formula.Recipe) error {
	return targetscope.PrepareLaunch(recipe, l.Scope, l.Members)
}

// invocationConvoyID reports the input convoy of a graph invocation, or empty
// for the shapes that have none.
//
// A launch with no convoy still gets a scope: §5 requires the stamp to be
// decoupled from the convoy, because stampGraphV2RootMetadata's convoy
// early-return is precisely how a no-convoy standalone graph root ends up
// unscoped.
func invocationConvoyID(inv graphv2.Invocation, isGraph bool) string {
	if !isGraph {
		return ""
	}
	return inv.InputConvoy
}
