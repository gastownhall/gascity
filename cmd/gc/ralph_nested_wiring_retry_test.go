package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// findSpawned returns the bead whose step ref ends in suffix (issue #9412
// AC-6). The inner control's own re-spawn derives its attempt refs from the
// control's compile-time gc.step_ref, which carries no formula prefix, so a
// re-spawned iteration is pinned by its ref suffix — the same dotted-suffix
// convention matchesScopeRef uses for scope membership.
func (r *wiringResult) findSpawned(t *testing.T, suffix string) beads.Bead {
	t.Helper()
	for _, b := range r.all {
		ref := beadRef(b)
		if ref == suffix || strings.HasSuffix(ref, "."+suffix) {
			return b
		}
	}
	t.Fatalf("no spawned bead with step ref ending %s", suffix)
	return beads.Bead{}
}

// TestRalphFinalizeNestedInnerRetriesThenPasses (issue #9412 AC-6): the inner
// check rejects its iteration 1 and passes its iteration 2 while the outer
// passes its iteration 1. The inner loop must re-spawn inner iteration 2 with
// its own scope (no gc.scope_ref on the scope body, members scoped to it),
// close the inner control pass, and only then let o1 run.
func TestRalphFinalizeNestedInnerRetriesThenPasses(t *testing.T) {
	res := runWiringWorkflow(t, wiringScenario{
		formula:       wiringNestedCheckTOML(true),
		checkPassIter: map[string]int{"outer": 1, "inner": 2},
	})

	const iter2 = "outer.iteration.1.inner.iteration.2"
	scope := res.findSpawned(t, iter2)
	ralphAssertOutcome(t, "inner iteration 2 scope", scope, "pass")
	if got, ok := scope.Metadata[beadmeta.ScopeRefMetadataKey]; ok && got != "" {
		t.Errorf("inner iteration 2 scope gc.scope_ref = %q, want none", got)
	}
	scopeRef := beadRef(scope)
	i1 := res.findSpawned(t, iter2+".i1")
	ralphAssertOutcome(t, "inner iteration 2 i1", i1, "pass")
	if got := i1.Metadata[beadmeta.ScopeRefMetadataKey]; got != scopeRef {
		t.Errorf("inner iteration 2 i1 gc.scope_ref = %q, want %q", got, scopeRef)
	}
	if got := i1.Metadata[beadmeta.RalphStepIDMetadataKey]; got != "inner" {
		t.Errorf("inner iteration 2 i1 gc.ralph_step_id = %q, want inner", got)
	}

	innerControl := res.get(t, "outer.iteration.1.inner")
	ralphAssertOutcome(t, "inner control", innerControl, "pass")
	o1 := res.get(t, "outer.iteration.1.o1")
	ralphAssertOutcome(t, "outer member o1", o1, "pass")
	if !o1.UpdatedAt.After(innerControl.UpdatedAt) {
		t.Errorf("o1 updated at %v, inner control at %v, want o1 later", o1.UpdatedAt, innerControl.UpdatedAt)
	}

	control := res.get(t, "outer")
	ralphAssertOutcome(t, "outer control", control, "pass")
	res.assertControlPassedBeforeFinalizer(t, control)
	ralphAssertOutcome(t, "workflow root", res.root(t), "pass")
	res.assertSourceFailureKeysEmpty(t)
	assertAllWorkflowBeadsClosed(t, res)
}
