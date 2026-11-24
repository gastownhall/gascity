package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

func nestedCheckStep(id string, children ...*formula.Step) *formula.Step {
	return &formula.Step{
		ID:       id,
		Title:    id,
		Type:     "task",
		Ralph:    &formula.RalphSpec{MaxAttempts: 2, Check: &formula.RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
		Children: children,
	}
}

func nestedPlainStep(id string, needs ...string) *formula.Step {
	return &formula.Step{ID: id, Title: id, Type: "task", Needs: needs}
}

// compileNestedCheckSteps runs the compile stages that shape loop membership
// (retries, checks, graph controls) and returns the flat step list.
func compileNestedCheckSteps(t *testing.T, steps []*formula.Step) []*formula.Step {
	t.Helper()
	retried, err := formula.ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries: %v", err)
	}
	expanded, err := formula.ApplyRalph(retried)
	if err != nil {
		t.Fatalf("ApplyRalph: %v", err)
	}
	f := &formula.Formula{Steps: expanded}
	formula.ApplyGraphControls(f)
	return f.Steps
}

// compiledStepBeads projects compiled steps onto the beads instantiation
// creates from them: the step's metadata, with gc.step_ref filled from the
// step ID where the compile left it empty.
func compiledStepBeads(steps []*formula.Step) []beads.Bead {
	out := make([]beads.Bead, 0, len(steps))
	for _, step := range steps {
		meta := make(map[string]string, len(step.Metadata)+1)
		for k, v := range step.Metadata {
			meta[k] = v
		}
		if meta[beadmeta.StepRefMetadataKey] == "" {
			meta[beadmeta.StepRefMetadataKey] = step.ID
		}
		out = append(out, beads.Bead{ID: step.ID, Metadata: meta})
	}
	return out
}

// TestNestedCheckScopeChecksResolveExactlyOneBody pins #9412 at the runtime
// boundary: every scope-check the compile mints for a check-with-children
// nested in a check body resolves, through matchesScopeRef, to exactly one
// scope body, so no scope-check can evaluate or close the wrong iteration.
func TestNestedCheckScopeChecksResolveExactlyOneBody(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		steps []*formula.Step
	}{
		{"two_level", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("inner", nestedPlainStep("i1")),
			nestedPlainStep("o1", "inner"),
		)}},
		{"three_level", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("mid", nestedCheckStep("inner", nestedPlainStep("i1"))),
		)}},
		{"retry_in_inner_body", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("inner", &formula.Step{ID: "r", Title: "r", Type: "task", Retry: &formula.RetrySpec{MaxAttempts: 2}}),
			nestedPlainStep("o1", "inner"),
		)}},
		{"o1_parallel", []*formula.Step{nestedCheckStep("outer",
			nestedCheckStep("inner", nestedPlainStep("i1")),
			nestedPlainStep("o1"),
		)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			all := compiledStepBeads(compileNestedCheckSteps(t, tc.steps))
			var bodies []beads.Bead
			for _, b := range all {
				if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindScope && b.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleBody {
					bodies = append(bodies, b)
				}
			}
			checks := 0
			for _, check := range all {
				if check.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindScopeCheck {
					continue
				}
				checks++
				scopeRef := check.Metadata[beadmeta.ScopeRefMetadataKey]
				var matched []string
				for _, body := range bodies {
					if matchesScopeRef(body, scopeRef) {
						matched = append(matched, body.ID)
					}
				}
				if len(matched) != 1 {
					t.Errorf("scope-check %s (gc.scope_ref=%q) matches bodies %v, want exactly one", check.ID, scopeRef, matched)
				}
			}
			if checks == 0 {
				t.Fatal("compile minted no scope-checks")
			}
		})
	}
}
