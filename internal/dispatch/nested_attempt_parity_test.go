package dispatch

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// parityKeys are the metadata a compile-time or re-spawned step must agree on
// across outer iterations (issue #9412 AC-4). A plain member's compile kind is
// empty while the runtime recipe defaults it to task; those are the same
// identity, so the kind is normalized.
func parityKeys(s *formula.Step) map[string]string {
	kind := s.Metadata[beadmeta.KindMetadataKey]
	if kind == "" {
		kind = beadmeta.KindTask
	}
	return map[string]string{
		"gc.kind":            kind,
		"gc.scope_role":      s.Metadata[beadmeta.ScopeRoleMetadataKey],
		"gc.step_id":         s.Metadata[beadmeta.StepIDMetadataKey],
		"gc.ralph_step_id":   s.Metadata[beadmeta.RalphStepIDMetadataKey],
		"gc.on_fail":         s.Metadata[beadmeta.OnFailMetadataKey],
		"gc.scope_ref_empty": fmt.Sprintf("%t", s.Metadata[beadmeta.ScopeRefMetadataKey] == ""),
	}
}

// assertParity compares the AC-4 key set of the compile-time iteration-1 step
// against the re-spawned iteration-2 step. label identifies the pair in
// failure output.
func assertParity(t *testing.T, label string, compileStep, runtimeStep *formula.Step) {
	t.Helper()
	want := parityKeys(compileStep)
	got := parityKeys(runtimeStep)
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("%s %s: compile = %v, runtime iteration 2 = %v", label, key, wantValue, got[key])
		}
	}
}

// TestNestedCheckIteration1CompileMatchesIteration2Recipe (issue #9412 AC-4):
// the inner loop's compile-time beads for outer iteration 1 must agree with
// what buildAttemptRecipe seeds for outer iteration 2, so iteration 1 and
// later iterations are the same shape. Before the fix, the compile stamped the
// inner scope and member with the outer iteration's identity while the
// re-spawn kept the inner loop's own.
func TestNestedCheckIteration1CompileMatchesIteration2Recipe(t *testing.T) {
	authored := dispatchNestedCheckSteps(true)

	// Compile side: outer iteration 1 as the compiler mints it.
	byID := compileNestedShape(t, authored)
	compilePairs := map[string]*formula.Step{
		"inner control":   byID["outer.iteration.1.inner"],
		"inner scope":     byID["outer.iteration.1.inner.iteration.1"],
		"inner member i1": byID["outer.iteration.1.inner.iteration.1.i1"],
		"i1 scope-check":  byID["outer.iteration.1.inner.iteration.1.i1-scope-check"],
		"outer scope":     byID["outer.iteration.1"],
	}
	for label, step := range compilePairs {
		if step == nil {
			t.Fatalf("compile missing %s", label)
		}
	}

	// Runtime side: outer iteration 2 as the re-spawn path builds it. The
	// frozen spec is the JSON round-trip of the authored outer step, exactly
	// what spawnNextAttempt deserializes from the spec bead.
	specJSON, err := json.Marshal(authored[0])
	if err != nil {
		t.Fatalf("marshaling frozen spec: %v", err)
	}
	var frozen formula.Step
	if err := json.Unmarshal(specJSON, &frozen); err != nil {
		t.Fatalf("unmarshaling frozen spec: %v", err)
	}
	control := beads.Bead{
		ID: "outer",
		Metadata: map[string]string{
			beadmeta.StepIDMetadataKey:  "outer",
			beadmeta.StepRefMetadataKey: "outer",
		},
	}
	recipe := buildAttemptRecipe(&frozen, control, 2)
	recipeByID := make(map[string]formula.RecipeStep, len(recipe.Steps))
	for _, s := range recipe.Steps {
		recipeByID[s.ID] = s
	}
	runtimePairs := map[string]formula.RecipeStep{
		"inner control":   recipeByID["outer.iteration.2.inner"],
		"inner scope":     recipeByID["outer.iteration.2.inner.iteration.1"],
		"inner member i1": recipeByID["outer.iteration.2.inner.iteration.1.i1"],
		"i1 scope-check":  recipeByID["outer.iteration.2.inner.iteration.1.i1-scope-check"],
		"outer scope":     recipeByID["outer.iteration.2"],
	}
	for label, step := range runtimePairs {
		if step.ID == "" {
			t.Fatalf("runtime iteration-2 recipe missing %s", label)
		}
	}

	for _, label := range []string{"inner control", "inner scope", "inner member i1", "i1 scope-check"} {
		compileStep := compilePairs[label]
		runtimeStep := runtimePairs[label]
		assertParity(t, label, compileStep, &formula.Step{Metadata: runtimeStep.Metadata})
	}

	// Each gc.scope_ref equals the gc.step_ref of the bead's innermost
	// enclosing iteration root: the inner member and its scope-check name the
	// inner iteration scope; the inner control names the outer iteration scope.
	for _, pair := range []struct {
		label  string
		member string
		root   string
	}{
		{label: "i1", member: "inner member i1", root: "inner scope"},
		{label: "i1 scope-check", member: "i1 scope-check", root: "inner scope"},
		{label: "inner control", member: "inner control", root: "outer scope"},
	} {
		if got := compilePairs[pair.member].Metadata[beadmeta.ScopeRefMetadataKey]; got != compilePairs[pair.root].Metadata[beadmeta.StepRefMetadataKey] {
			t.Errorf("compile %s gc.scope_ref = %q, want enclosing root step ref %q", pair.label, got, compilePairs[pair.root].Metadata[beadmeta.StepRefMetadataKey])
		}
		if got := runtimePairs[pair.member].Metadata[beadmeta.ScopeRefMetadataKey]; got != runtimePairs[pair.root].Metadata[beadmeta.StepRefMetadataKey] {
			t.Errorf("runtime %s gc.scope_ref = %q, want enclosing root step ref %q", pair.label, got, runtimePairs[pair.root].Metadata[beadmeta.StepRefMetadataKey])
		}
	}
}
