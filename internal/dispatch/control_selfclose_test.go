package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// TestAttemptRecipeNeverMintsSelfClosingControlEdges pins the attempt re-mint
// site against the same structural invariant the compiler enforces: an attempt
// scope body must never come back blocked by a scope-check that closes it
// (ga-a6zy9). applyAttemptRecipeScopeChecks is the second producer of the
// rewrite that minted the live deadlocks.
func TestAttemptRecipeNeverMintsSelfClosingControlEdges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		step    *formula.Step
		attempt int
	}{
		{
			name: "ralph iteration with a linear child chain",
			step: &formula.Step{
				ID:    "converge",
				Title: "Converge",
				Type:  "task",
				Ralph: &formula.RalphSpec{MaxAttempts: 5},
				Children: []*formula.Step{
					{ID: "apply", Title: "Apply", Type: "task"},
					{ID: "verify", Title: "Verify", Type: "task", Needs: []string{"apply"}},
				},
			},
			attempt: 3,
		},
		{
			name: "review fan-in with retry-managed children",
			step: &formula.Step{
				ID:    "review-loop",
				Title: "Review loop",
				Ralph: &formula.RalphSpec{MaxAttempts: 999},
				Children: []*formula.Step{
					{ID: "review-claude", Title: "Claude review", Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"}},
					{ID: "review-codex", Title: "Codex review", Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"}},
					{ID: "synthesize", Title: "Synthesize", Needs: []string{"review-claude", "review-codex"}},
					{ID: "apply-fixes", Title: "Apply fixes", Needs: []string{"synthesize"}},
				},
			},
			attempt: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			control := beads.Bead{
				ID: "ctrl-1",
				Metadata: map[string]string{
					"gc.step_id":  tc.step.ID,
					"gc.step_ref": "mol-test." + tc.step.ID,
				},
			}
			recipe := buildAttemptRecipe(tc.step, control, tc.attempt)

			if found := formula.FindSelfClosingControlEdges(recipe.Steps, recipe.Deps); len(found) != 0 {
				t.Fatalf("attempt recipe minted self-closing control edges: %v", found)
			}

			// The legitimate rewrite must survive: a downstream child still
			// waits on its predecessor's scope-check, not on the raw step.
			scopeChecks := 0
			for _, step := range recipe.Steps {
				if step.Metadata["gc.kind"] == "scope-check" {
					scopeChecks++
				}
			}
			if scopeChecks == 0 {
				t.Fatal("no scope-checks minted; the fixture proves nothing about the rewrite")
			}
			rewritten := false
			for _, dep := range recipe.Deps {
				if dep.StepID != recipe.Name && dep.Type == "blocks" {
					if check := recipe.StepByID(dep.DependsOnID); check != nil && check.Metadata["gc.kind"] == "scope-check" {
						rewritten = true
					}
				}
			}
			if !rewritten {
				t.Fatalf("no non-body dep was rewritten to a scope-check; deps = %+v", recipe.Deps)
			}
		})
	}
}
