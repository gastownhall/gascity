package sling

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/targetscope"
)

// scopeTestDeps builds deps whose rig supplies a stored default branch, so the
// routing layer has something to inject without touching a real git repo.
func scopeTestDeps(t *testing.T, rig config.Rig) SlingDeps {
	t.Helper()
	return testDeps(&config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs:      []config.Rig{rig},
	}, runtime.NewFake(), newFakeRunner().run)
}

func scopeAgent() config.Agent {
	return config.Agent{Name: "polecat", Dir: "hw"}
}

// The whole point of the separate sources builder: the layers arrive APART.
// A merged map cannot answer "did base_branch come from the operator or from
// the rig", and precedence is decided per layer.
func TestScopeSourcesKeepsLayersSeparate(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{
		Name:        "hw",
		FormulaVars: map[string]string{"base_branch": "rig-branch"},
	})

	src := SlingFormulaScopeSources("mol-scoped-work", "", []string{"base_branch=explicit-branch"}, scopeAgent(), deps)

	if len(src.Explicit) != 1 || src.Explicit[0] != "base_branch=explicit-branch" {
		t.Fatalf("Explicit = %v, want the raw unflattened operator var", src.Explicit)
	}
	if got := src.Rig["base_branch"]; got != "rig-branch" {
		t.Fatalf("Rig[base_branch] = %q, want rig-branch (the rig layer must survive un-merged)", got)
	}
}

// §11 #16 — explicit gc_target_branch beats a routed base_branch, and the
// consumed carrier is rewritten to the winner so template substitution and the
// close gate validate against the SAME branch.
func TestScopeSourcesExplicitTargetBranchBeatsRoutingAndRewritesCarrier(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{Name: "hw", DefaultBranch: "main"})

	src := SlingFormulaScopeSources("mol-scoped-work", "", []string{"gc_target_branch=release"}, scopeAgent(), deps)

	if got := src.Routing[targetscope.VarBaseBranch]; got != "main" {
		t.Fatalf("Routing[base_branch] = %q, want the routed main", got)
	}

	resolved, err := targetscope.Resolve(src, "/store")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Scope.Branch != "release" {
		t.Fatalf("scope.Branch = %q, want release (explicit outranks routing)", resolved.Scope.Branch)
	}

	vars := BuildSlingFormulaVars("mol-scoped-work", "", []string{"gc_target_branch=release"}, scopeAgent(), deps)
	targetscope.ApplyToVars(vars, resolved, SlingFormulaUsesBaseBranch("mol-scoped-work"), SlingFormulaUsesTargetBranch("mol-scoped-work"))

	if vars["base_branch"] != resolved.Scope.Branch {
		t.Fatalf("vars[base_branch] = %q but scope.Branch = %q; the consumed carrier and the scope must be equal by construction",
			vars["base_branch"], resolved.Scope.Branch)
	}
	if _, leaked := vars["gc_target_branch"]; leaked {
		t.Fatal("gc_target_branch leaked into the consumed map; scope inputs are claimed by the resolver")
	}
}

// §11 #16 empty-carrier case: an explicitly emptied carrier is not a veto and
// not a survivor — it is overwritten by the reconciled winner.
func TestScopeSourcesEmptyCarrierIsOverwrittenByWinner(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{Name: "hw", DefaultBranch: "main"})
	userVars := []string{"base_branch=", "gc_target_branch=release"}

	src := SlingFormulaScopeSources("mol-scoped-work", "", userVars, scopeAgent(), deps)
	resolved, err := targetscope.Resolve(src, "/store")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	vars := BuildSlingFormulaVars("mol-scoped-work", "", userVars, scopeAgent(), deps)
	targetscope.ApplyToVars(vars, resolved, true, false)

	if vars["base_branch"] != "release" {
		t.Fatalf("vars[base_branch] = %q, want release (not left empty, not the routed main)", vars["base_branch"])
	}
}

// §11 #16 within-layer conflict: two aliases of one fact disagreeing at the
// same layer is an operator error with no tiebreak.
func TestScopeSourcesRejectsWithinLayerCarrierConflict(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{Name: "hw", DefaultBranch: "main"})

	src := SlingFormulaScopeSources("mol-scoped-work", "", []string{"base_branch=X", "target_branch=Y"}, scopeAgent(), deps)

	if _, err := targetscope.Resolve(src, "/store"); err == nil {
		t.Fatal("Resolve accepted disagreeing explicit carriers; want a rejection")
	} else if !strings.Contains(err.Error(), "conflicting target branch") {
		t.Fatalf("err = %v, want a conflicting-target-branch rejection", err)
	}
}

// A doubled carrier must survive as a conflict rather than being flattened to
// last-write-wins before the resolver ever sees it.
func TestScopeSourcesRejectsDuplicateExplicitCarrier(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{Name: "hw"})

	src := SlingFormulaScopeSources("mol-scoped-work", "", []string{"base_branch=X", "base_branch=Y"}, scopeAgent(), deps)

	if _, err := targetscope.Resolve(src, "/store"); err == nil {
		t.Fatal("Resolve accepted a doubled explicit carrier with different values; want a rejection")
	}
}

// §11 #16 default-only: with no operator or routed branch, the formula default
// supplies the scope — which is why resolution cannot happen before the formula
// is loaded.
func TestScopeSourcesDefaultOnlyComesFromFormulaLayer(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{Name: "hw"})

	src := SlingFormulaScopeSources("mol-scoped-work", "", nil, scopeAgent(), deps)
	if src.Routing != nil {
		t.Fatalf("Routing = %v, want nil when no branch resolves", src.Routing)
	}

	src.FormulaDefaults = map[string]string{"base_branch": "main"}
	resolved, err := targetscope.Resolve(src, "/store")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Scope.Branch != "main" {
		t.Fatalf("scope.Branch = %q, want the formula default main", resolved.Scope.Branch)
	}
	if resolved.BranchLayer != targetscope.LayerFormula {
		t.Fatalf("BranchLayer = %q, want the formula layer", resolved.BranchLayer)
	}
}

// A formula the name predicates do not match receives no routed branch today.
// The faithful answer is an empty routing layer, so the formula default decides
// rather than a branch the injector never actually supplied.
func TestScopeSourcesUnmatchedFormulaGetsNoRoutedBranch(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{Name: "hw", DefaultBranch: "main"})

	src := SlingFormulaScopeSources("acme-work", "", nil, scopeAgent(), deps)

	if src.Routing != nil {
		t.Fatalf("Routing = %v, want nil for a formula routing does not inject into", src.Routing)
	}
}

// The rig layer is read through the same rig resolution the merge uses, so the
// sources builder and the vars builder cannot disagree about which rig applies.
func TestScopeSourcesRigLayerMatchesMergedVars(t *testing.T) {
	deps := scopeTestDeps(t, config.Rig{
		Name:        "hw",
		FormulaVars: map[string]string{"base_branch": "rig-branch"},
	})

	src := SlingFormulaScopeSources("mol-scoped-work", "", nil, scopeAgent(), deps)
	vars := BuildSlingFormulaVars("mol-scoped-work", "", nil, scopeAgent(), deps)

	// Non-vacuity: two empty lookups would satisfy the equality below while
	// proving nothing, which is precisely how this test could go false-green.
	if src.Rig["base_branch"] == "" {
		t.Fatal("rig layer is empty; the test would compare two blanks and assert nothing")
	}
	if src.Rig["base_branch"] != vars["base_branch"] {
		t.Fatalf("rig layer %q != merged vars %q; the two rig lookups drifted",
			src.Rig["base_branch"], vars["base_branch"])
	}
}
