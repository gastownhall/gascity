package sling

import (
	"fmt"

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

	resolved, err := targetscope.Resolve(sources, SlingFormulaRepoDir(beadID, deps, a))
	if err != nil {
		return LaunchScope{}, fmt.Errorf("resolving target scope for %s: %w", formulaName, err)
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

// formulaDeclaresVar reports whether the resolved formula declares a variable,
// which is what makes it a carrier this formula actually consumes.
func formulaDeclaresVar(f *formula.Formula, name string) bool {
	if f == nil || len(f.Vars) == 0 {
		return false
	}
	_, ok := f.Vars[name]
	return ok
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
