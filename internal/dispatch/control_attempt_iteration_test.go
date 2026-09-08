package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// gc.attempt answers "which attempt of THIS step is this". A ralph iteration
// root is a step whose attempts are iterations, so attempt == iteration there.
// Its body children are DIFFERENT steps: each owns a retry counter that starts
// at 1 and is bounded by that child's own gc.max_attempts. Stamping the outer
// iteration onto a child conflates the two counters, and the conflation is not
// cosmetic — processRetryEval reads gc.attempt off the child and computes
// nextAttempt = attempt + 1, so a child first run in iteration N is born at
// attempt N against max_attempts 3. By iteration 3 it is exhausted before it
// runs and hard-fails with zero retries taken. Live census of the
// maintainer-city graph store found 81 of 382 retry-kind beads (21%) born at or
// past their own max, including 40 at attempt 4 and 10 at attempt 5 against a
// max of 3 — values a counter that starts at 1 can never reach (ga-v7pu5).
//
// The iteration index is still worth recording; it just needs its own key.
// gc.iteration has existed in beadmeta since the runs view started reading it
// (internal/runproj/detail_nodeshape.go) but nothing ever wrote it.
func TestBuildAttemptRecipeKeepsChildRetryCountersIndependentOfIteration(t *testing.T) {
	t.Parallel()

	// The frozen spec shape observed live: by the time a ralph step is frozen,
	// its children have already been through ApplyRetries, so the child list
	// holds the retry control, its spec, and its first attempt bead — and that
	// attempt bead carries the gc.attempt=1 expandRetry stamped on it.
	newStep := func() *formula.Step {
		return &formula.Step{
			ID:    "review-loop",
			Title: "Review loop",
			Type:  "task",
			Ralph: &formula.RalphSpec{
				MaxAttempts: 5,
				Check:       &formula.RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: []*formula.Step{
				{
					ID:    "scorecard",
					Title: "Scorecard",
					Type:  "task",
					Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
				},
				{
					ID:    "scorecard.attempt.1",
					Title: "Scorecard",
					Type:  "task",
					Metadata: map[string]string{
						"gc.attempt":     "1",
						"gc.control_for": "scorecard",
					},
				},
				{ID: "publish", Title: "Publish", Type: "task"},
			},
		}
	}

	control := beads.Bead{
		ID: "gc-1",
		Metadata: map[string]string{
			"gc.step_id":  "review-loop",
			"gc.step_ref": "mol-test.review-loop",
		},
	}

	recipe := buildAttemptRecipe(newStep(), control, 3)

	stepByID := func(t *testing.T, id string) *formula.RecipeStep {
		t.Helper()
		step := recipe.StepByID(id)
		if step == nil {
			ids := make([]string, 0, len(recipe.Steps))
			for _, s := range recipe.Steps {
				ids = append(ids, s.ID)
			}
			t.Fatalf("missing step %q; recipe has %v", id, ids)
		}
		return step
	}

	t.Run("retry control starts its own counter at 1", func(t *testing.T) {
		// This is the bead processRetryEval reads. Born at the iteration index,
		// it spends the child's retry budget on iterations it never used.
		got := stepByID(t, "mol-test.review-loop.iteration.3.scorecard")
		if attempt := got.Metadata["gc.attempt"]; attempt != "1" {
			t.Errorf("retry control gc.attempt = %q, want 1 (its own counter, not the iteration)", attempt)
		}
		if kind := got.Metadata["gc.kind"]; kind != "retry" {
			t.Fatalf("control gc.kind = %q, want retry — the fixture is not exercising a retry control", kind)
		}
		if maximum := got.Metadata["gc.max_attempts"]; maximum != "3" {
			t.Errorf("retry control gc.max_attempts = %q, want 3", maximum)
		}
	})

	t.Run("first attempt bead keeps the attempt its spec carries", func(t *testing.T) {
		got := stepByID(t, "mol-test.review-loop.iteration.3.scorecard.attempt.1")
		if attempt := got.Metadata["gc.attempt"]; attempt != "1" {
			t.Errorf("gc.attempt = %q, want 1 — the ref says attempt.1, so the metadata must agree", attempt)
		}
	})

	t.Run("plain children keep the existing iteration stamp", func(t *testing.T) {
		// A child with no retry control has no counter of its own, so nothing
		// reads its gc.attempt. TestBuildAttemptRecipeRalphWithChildren pins the
		// current value; this change must not disturb it.
		got := stepByID(t, "mol-test.review-loop.iteration.3.publish")
		if attempt := got.Metadata["gc.attempt"]; attempt != "3" {
			t.Errorf("plain child gc.attempt = %q, want 3", attempt)
		}
	})

	t.Run("every step records the iteration under its own key", func(t *testing.T) {
		for _, id := range []string{
			"mol-test.review-loop.iteration.3",
			"mol-test.review-loop.iteration.3.scorecard",
			"mol-test.review-loop.iteration.3.scorecard.attempt.1",
			"mol-test.review-loop.iteration.3.publish",
		} {
			if iteration := stepByID(t, id).Metadata["gc.iteration"]; iteration != "3" {
				t.Errorf("%s: gc.iteration = %q, want 3", id, iteration)
			}
		}
	})

	t.Run("retry children point at THIS iteration's control", func(t *testing.T) {
		// S38 gave nested ralph controls a namespaced gc.control_for so an
		// iteration resolves only its own attempt roots. Nested RETRY controls
		// never got it: their attempt roots come from the frozen spec, whose
		// gc.control_for is the bare child id, and the copy loop below passes it
		// through unchanged. Live molecules show every iteration's attempt.1
		// carrying the identical bare "review-pipeline.quality-scorecard", so
		// all of them match every iteration's control through the shared
		// gc.step_id identity member. Only max(gc.attempt) kept them apart —
		// which worked solely because the iteration index was being stamped
		// there, the very conflation this file removes.
		got := stepByID(t, "mol-test.review-loop.iteration.3.scorecard.attempt.1")
		want := "mol-test.review-loop.iteration.3.scorecard"
		if cf := got.Metadata["gc.control_for"]; cf != want {
			t.Errorf("gc.control_for = %q, want %q (this iteration's control, not the bare step id)", cf, want)
		}
	})

	t.Run("iteration root still counts iterations in gc.attempt", func(t *testing.T) {
		// The root IS the ralph subject, and processRalphControl reads gc.attempt
		// off it. Its attempts genuinely are iterations, so this must not move.
		got := stepByID(t, "mol-test.review-loop.iteration.3")
		if attempt := got.Metadata["gc.attempt"]; attempt != "3" {
			t.Errorf("iteration root gc.attempt = %q, want 3", attempt)
		}
	})
}

// TestBuildAttemptRecipeRetryAttemptsStillCarryTheAttemptNumber guards the
// non-ralph half. A plain retry's sub-DAG really is attempt N, so its root and
// any children it owns must keep counting attempts — the fix above is scoped to
// ralph iterations and must not leak into this path.
func TestBuildAttemptRecipeRetryAttemptsStillCarryTheAttemptNumber(t *testing.T) {
	t.Parallel()

	step := &formula.Step{
		ID:    "finalize",
		Title: "Finalize",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 3},
		Children: []*formula.Step{
			{ID: "inner", Title: "Inner", Type: "task"},
		},
	}
	control := beads.Bead{
		ID: "gc-2",
		Metadata: map[string]string{
			"gc.step_id":  "finalize",
			"gc.step_ref": "mol-test.finalize",
		},
	}

	recipe := buildAttemptRecipe(step, control, 2)

	root := recipe.StepByID("mol-test.finalize.attempt.2")
	if root == nil {
		t.Fatal("missing attempt root")
	}
	if attempt := root.Metadata["gc.attempt"]; attempt != "2" {
		t.Errorf("attempt root gc.attempt = %q, want 2", attempt)
	}
	if iteration, ok := root.Metadata["gc.iteration"]; ok {
		t.Errorf("attempt root carries gc.iteration = %q; a plain retry has no iteration", iteration)
	}

	inner := recipe.StepByID("mol-test.finalize.attempt.2.inner")
	if inner == nil {
		t.Fatal("missing inner child")
	}
	if attempt := inner.Metadata["gc.attempt"]; attempt != "2" {
		t.Errorf("inner child gc.attempt = %q, want 2 — it is part of attempt 2", attempt)
	}
}

// TestBuildAttemptRecipeInheritsIterationForNestedRetryAttempts covers the path
// that actually writes the review artifacts. A retry nested inside a ralph body
// spawns through the NON-ralph branch — step.Ralph is nil for the child — so it
// cannot derive the iteration from attemptNum. It has to carry the iteration
// forward from its control, or every retried step inside an iteration loses the
// only stable name for the directory its siblings wrote to (ga-la0py).
func TestBuildAttemptRecipeInheritsIterationForNestedRetryAttempts(t *testing.T) {
	t.Parallel()

	step := &formula.Step{
		ID:    "scorecard",
		Title: "Scorecard",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 3},
	}
	// The control is the retry control bead minted as a child of iteration 2,
	// so it carries that iteration.
	control := beads.Bead{
		ID: "gc-3",
		Metadata: map[string]string{
			"gc.step_id":   "scorecard",
			"gc.step_ref":  "mol-test.review-loop.iteration.2.scorecard",
			"gc.iteration": "2",
		},
	}

	recipe := buildAttemptRecipe(step, control, 2)

	root := recipe.StepByID("mol-test.review-loop.iteration.2.scorecard.attempt.2")
	if root == nil {
		ids := make([]string, 0, len(recipe.Steps))
		for _, s := range recipe.Steps {
			ids = append(ids, s.ID)
		}
		t.Fatalf("missing attempt root; recipe has %v", ids)
	}
	if iteration := root.Metadata["gc.iteration"]; iteration != "2" {
		t.Errorf("gc.iteration = %q, want 2 — inherited from the control, not from attemptNum", iteration)
	}
	// The two counters have genuinely diverged here: this is retry attempt 2 of
	// a step inside iteration 2. Only coincidence makes them equal; the point is
	// that each is now readable on its own.
	if attempt := root.Metadata["gc.attempt"]; attempt != "2" {
		t.Errorf("gc.attempt = %q, want 2", attempt)
	}
}

// TestLatestAttemptPrefersPreciseControlIdentityOverBareStepID is the read-side
// half of the counter split. A control's identity set has three members, and
// they are not equally specific: the bead ID and the namespaced gc.step_ref name
// exactly one control, while the bare gc.step_id names the same inner control in
// EVERY outer iteration. Attempt roots minted before the namespaced stamp
// existed carry only the bare form, so a molecule that spans a deploy holds both
// shapes at once.
//
// Until now max(gc.attempt) hid the ambiguity, because a body child was stamped
// with its outer iteration index and later iterations therefore always scored
// higher. Once each child counts its own attempts from 1, the older sibling's
// stale index outranks the current iteration's first attempt and the control
// resolves a closed attempt from an iteration it has nothing to do with.
// Precision has to be ranked explicitly rather than inferred from a number that
// no longer means what it used to (S38, ga-v7pu5).
func TestLatestAttemptPrefersPreciseControlIdentityOverBareStepID(t *testing.T) {
	t.Parallel()

	control := beads.Bead{
		ID: "gc-ctl-iter5",
		Metadata: map[string]string{
			"gc.kind":     "retry",
			"gc.step_ref": "mol.review-loop.iteration.5.scorecard",
			"gc.step_id":  "scorecard",
		},
	}

	candidates := []beads.Bead{
		// Minted before the deploy, in earlier iterations of this same loop:
		// bare stamp, and carrying the outer iteration index as its attempt.
		{ID: "old-iter2", Metadata: map[string]string{
			"gc.control_for": "scorecard",
			"gc.attempt":     "2",
		}},
		{ID: "old-iter3", Metadata: map[string]string{
			"gc.control_for": "scorecard",
			"gc.attempt":     "3",
		}},
		// This iteration's own first attempt, minted after the split.
		{ID: "new-iter5", Metadata: map[string]string{
			"gc.control_for": "mol.review-loop.iteration.5.scorecard",
			"gc.attempt":     "1",
		}},
	}

	if got := latestAttemptFromCandidates(control, candidates); got.ID != "new-iter5" {
		t.Errorf("resolved %q, want new-iter5 — a precise stamp outranks a bare one regardless of gc.attempt", got.ID)
	}

	t.Run("bare stamps still resolve when nothing precise exists", func(t *testing.T) {
		// A molecule minted entirely before the namespaced stamp has only bare
		// candidates. Those must keep resolving exactly as they do today.
		if got := latestAttemptFromCandidates(control, candidates[:2]); got.ID != "old-iter3" {
			t.Errorf("resolved %q, want old-iter3 — bare candidates still select max(gc.attempt)", got.ID)
		}
	})

	t.Run("infrastructure is filtered before precision is judged", func(t *testing.T) {
		// A scope-check carries the precise ref too. It must not count as the
		// precise match and starve the real attempt root of its bare fallback.
		withCheck := append([]beads.Bead{{ID: "chk", Metadata: map[string]string{
			"gc.control_for": "mol.review-loop.iteration.5.scorecard",
			"gc.attempt":     "9",
			"gc.kind":        "scope-check",
		}}}, candidates[:2]...)
		if got := latestAttemptFromCandidates(control, withCheck); got.ID != "old-iter3" {
			t.Errorf("resolved %q, want old-iter3 — a scope-check is not an attempt root", got.ID)
		}
	})
}
