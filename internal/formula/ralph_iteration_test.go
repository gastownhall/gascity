package formula

import "testing"

// Iteration 1 is minted here by ApplyRalph; iterations 2+ are minted at runtime
// by dispatch.buildAttemptRecipe from the frozen spec. Those are two code paths
// producing beads that downstream consumers — the retry evaluator, the runs
// view, and the artifact-path resolver — read identically. Every defect in this
// family is the two paths disagreeing, so the contract worth pinning is that
// iteration 1 already carries what iterations 2+ carry.
func TestApplyRalph_IterationOneStampsIterationAndPreservesChildAttempts(t *testing.T) {
	// The real pipeline order: retries expand on the children first, so ralph
	// receives a body that already contains a retry control, its spec, and its
	// first attempt bead.
	expandedChildren, err := ApplyRetries([]*Step{
		{
			ID:    "scorecard",
			Title: "Scorecard",
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
		{
			ID:    "publish",
			Title: "Publish",
			Needs: []string{"scorecard"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyRetries: %v", err)
	}

	got, err := ApplyRalph([]*Step{
		{
			ID:    "review-loop",
			Title: "Review loop",
			Type:  "task",
			Ralph: &RalphSpec{
				MaxAttempts: 3,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: expandedChildren,
		},
	})
	if err != nil {
		t.Fatalf("ApplyRalph: %v", err)
	}

	byID := make(map[string]*Step, len(got))
	for _, step := range got {
		byID[step.ID] = step
	}
	mustGet := func(t *testing.T, id string) *Step {
		t.Helper()
		step, ok := byID[id]
		if !ok {
			ids := make([]string, 0, len(got))
			for _, s := range got {
				ids = append(ids, s.ID)
			}
			t.Fatalf("missing step %q; got %v", id, ids)
		}
		return step
	}

	t.Run("iteration scope and body children record the iteration", func(t *testing.T) {
		for _, id := range []string{
			"review-loop.iteration.1",
			"review-loop.iteration.1.scorecard",
			"review-loop.iteration.1.scorecard.attempt.1",
			"review-loop.iteration.1.publish",
		} {
			if iteration := mustGet(t, id).Metadata["gc.iteration"]; iteration != "1" {
				t.Errorf("%s: gc.iteration = %q, want 1", id, iteration)
			}
		}
	})

	t.Run("retry control keeps its own counter", func(t *testing.T) {
		// Trivially true while iteration 1 is the only compile-time iteration,
		// since both counters read 1 — but it is the same rule the runtime path
		// applies at iteration 3, and the two must not drift apart.
		control := mustGet(t, "review-loop.iteration.1.scorecard")
		if kind := control.Metadata["gc.kind"]; kind != "retry" {
			t.Fatalf("gc.kind = %q, want retry — the fixture is not exercising a retry control", kind)
		}
		if attempt := control.Metadata["gc.attempt"]; attempt != "1" {
			t.Errorf("retry control gc.attempt = %q, want 1", attempt)
		}
		if attempt := mustGet(t, "review-loop.iteration.1.scorecard.attempt.1").Metadata["gc.attempt"]; attempt != "1" {
			t.Errorf("first attempt gc.attempt = %q, want 1", attempt)
		}
	})
}

// TestRalphBodyChildAttempt pins the shared rule directly, at the iteration
// numbers the compile-time path can never reach. dispatch.buildAttemptRecipe
// calls this for every ralph body child on iterations 2+, which is where the
// two counters actually diverge.
func TestRalphBodyChildAttempt(t *testing.T) {
	tests := []struct {
		name  string
		child *Step
		want  string
	}{
		{
			name:  "retry control starts its own counter",
			child: &Step{ID: "scorecard", Retry: &RetrySpec{MaxAttempts: 3}},
			want:  "1",
		},
		{
			name:  "nested ralph control starts its own counter",
			child: &Step{ID: "inner", Ralph: &RalphSpec{MaxAttempts: 3}},
			want:  "1",
		},
		{
			name:  "expanded attempt keeps the attempt its spec carries",
			child: &Step{ID: "scorecard.attempt.1", Metadata: map[string]string{"gc.attempt": "1"}},
			want:  "1",
		},
		{
			name:  "plain body step inherits the iteration",
			child: &Step{ID: "publish"},
			want:  "4",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RalphBodyChildAttempt(tc.child, 4); got != tc.want {
				t.Errorf("RalphBodyChildAttempt(%s, 4) = %q, want %q", tc.child.ID, got, tc.want)
			}
		})
	}
}
