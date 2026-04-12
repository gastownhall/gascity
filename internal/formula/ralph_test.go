package formula

import "testing"

func TestApplyRalph_Basic(t *testing.T) {
	steps := []*Step{
		{
			ID:          "implement",
			Title:       "Implement widget",
			Description: "Make the code changes.",
			Type:        "task",
			DependsOn:   []string{"design"},
			Needs:       []string{"setup"},
			Labels:      []string{"frontend"},
			Metadata: map[string]string{
				"custom": "value",
			},
			Ralph: &RalphSpec{
				MaxAttempts: 3,
				Check: &RalphCheckSpec{
					Mode:    "exec",
					Path:    ".gascity/checks/widget.sh",
					Timeout: "2m",
				},
			},
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (control + spec + iteration)", len(got))
	}

	control := got[0]
	spec := got[1]
	iteration := got[2]

	// Control bead.
	if control.ID != "implement" {
		t.Fatalf("control.ID = %q, want implement", control.ID)
	}
	if control.Metadata["gc.kind"] != "ralph" {
		t.Errorf("control gc.kind = %q, want ralph", control.Metadata["gc.kind"])
	}
	if control.Metadata["gc.max_attempts"] != "3" {
		t.Errorf("control gc.max_attempts = %q, want 3", control.Metadata["gc.max_attempts"])
	}
	if control.Metadata["gc.check_mode"] != "exec" {
		t.Errorf("control gc.check_mode = %q, want exec", control.Metadata["gc.check_mode"])
	}
	if control.Metadata["gc.check_path"] != ".gascity/checks/widget.sh" {
		t.Errorf("control gc.check_path = %q, want .gascity/checks/widget.sh", control.Metadata["gc.check_path"])
	}
	if control.Metadata["gc.check_timeout"] != "2m" {
		t.Errorf("control gc.check_timeout = %q, want 2m", control.Metadata["gc.check_timeout"])
	}
	if control.Metadata["gc.control_epoch"] != "1" {
		t.Errorf("control gc.control_epoch = %q, want 1", control.Metadata["gc.control_epoch"])
	}
	if control.Metadata["gc.source_step_spec"] != "" {
		t.Fatalf("control gc.source_step_spec = %q, want empty", control.Metadata["gc.source_step_spec"])
	}
	assertFrozenSpecStep(t, spec, "implement", nil)

	// Control blocks on the iteration (not a check bead).
	wantControlNeeds := map[string]bool{"setup": true, "implement.iteration.1": true}
	if len(control.Needs) != 2 {
		t.Fatalf("control.Needs = %v, want two entries", control.Needs)
	}
	for _, need := range control.Needs {
		if !wantControlNeeds[need] {
			t.Errorf("control.Needs contains unexpected %q", need)
		}
	}

	// Iteration bead.
	if iteration.ID != "implement.iteration.1" {
		t.Fatalf("iteration.ID = %q, want implement.iteration.1", iteration.ID)
	}
	if iteration.Metadata["gc.attempt"] != "1" {
		t.Errorf("iteration gc.attempt = %q, want 1", iteration.Metadata["gc.attempt"])
	}
	if iteration.Metadata["gc.ralph_step_id"] != "implement" {
		t.Errorf("iteration gc.ralph_step_id = %q, want implement", iteration.Metadata["gc.ralph_step_id"])
	}
	if iteration.Metadata["custom"] != "value" {
		t.Errorf("iteration custom metadata = %q, want value", iteration.Metadata["custom"])
	}

	// Iteration inherits external deps.
	if len(iteration.DependsOn) != 1 || iteration.DependsOn[0] != "design" {
		t.Errorf("iteration.DependsOn = %v, want [design]", iteration.DependsOn)
	}
	if len(iteration.Needs) != 1 || iteration.Needs[0] != "setup" {
		t.Errorf("iteration.Needs = %v, want [setup]", iteration.Needs)
	}
}

func TestApplyRalph_FrozenSpecRoundTrips(t *testing.T) {
	original := &Step{
		ID:    "converge",
		Title: "Converge",
		Type:  "task",
		Ralph: &RalphSpec{
			MaxAttempts: 5,
			Check: &RalphCheckSpec{
				Mode: "exec",
				Path: "check.sh",
			},
		},
		Children: []*Step{
			{ID: "apply", Title: "Apply changes"},
		},
	}

	got, err := ApplyRalph([]*Step{original})
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	assertFrozenSpecStep(t, got[1], "converge", func(frozen Step) {
		if frozen.Ralph == nil || frozen.Ralph.MaxAttempts != 5 {
			t.Errorf("frozen ralph = %+v, want max_attempts=5", frozen.Ralph)
		}
		if len(frozen.Children) != 1 || frozen.Children[0].ID != "apply" {
			t.Errorf("frozen children = %v, want [apply]", frozen.Children)
		}
	})
}

func TestApplyRalph_NestedWithChildren(t *testing.T) {
	steps := []*Step{
		{
			ID:    "review-loop",
			Title: "Review loop",
			Type:  "task",
			Ralph: &RalphSpec{
				MaxAttempts: 3,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: []*Step{
				{ID: "review", Title: "Review"},
				{ID: "apply", Title: "Apply", Needs: []string{"review"}},
			},
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	// Expect: control + spec + iteration scope + 2 body children = 5
	if len(got) != 5 {
		names := make([]string, len(got))
		for i, s := range got {
			names[i] = s.ID
		}
		t.Fatalf("len(got) = %d, want 5; steps: %v", len(got), names)
	}

	control := got[0]
	iteration := got[2]

	if control.Metadata["gc.kind"] != "ralph" {
		t.Errorf("control gc.kind = %q, want ralph", control.Metadata["gc.kind"])
	}
	if iteration.Metadata["gc.kind"] != "scope" {
		t.Errorf("iteration gc.kind = %q, want scope", iteration.Metadata["gc.kind"])
	}
	if iteration.ID != "review-loop.iteration.1" {
		t.Errorf("iteration.ID = %q, want review-loop.iteration.1", iteration.ID)
	}

	// Body children should be namespaced under the iteration.
	review := got[3]
	apply := got[4]
	if review.ID != "review-loop.iteration.1.review" {
		t.Errorf("review.ID = %q, want review-loop.iteration.1.review", review.ID)
	}
	if apply.ID != "review-loop.iteration.1.apply" {
		t.Errorf("apply.ID = %q, want review-loop.iteration.1.apply", apply.ID)
	}

	// apply should depend on review (namespaced).
	if len(apply.Needs) < 1 {
		t.Fatalf("apply.Needs = %v, want at least review-loop.iteration.1.review", apply.Needs)
	}
	foundReviewDep := false
	for _, n := range apply.Needs {
		if n == "review-loop.iteration.1.review" {
			foundReviewDep = true
		}
	}
	if !foundReviewDep {
		t.Errorf("apply.Needs = %v, missing review-loop.iteration.1.review", apply.Needs)
	}
}

func TestApplyRalph_BodyStepsHaveNamespacedStepRef(t *testing.T) {
	steps := []*Step{
		{
			ID:    "review-loop",
			Title: "Review loop",
			Type:  "task",
			Ralph: &RalphSpec{
				MaxAttempts: 3,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: []*Step{
				{ID: "review", Title: "Review"},
				{ID: "apply", Title: "Apply", Needs: []string{"review"}},
			},
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	// Iteration/body steps (after control + spec) should have gc.step_ref matching their namespaced ID.
	for _, step := range got[2:] {
		ref := step.Metadata["gc.step_ref"]
		if ref != step.ID {
			t.Errorf("step %q: gc.step_ref = %q, want %q", step.ID, ref, step.ID)
		}
	}
}

func TestApplyRalph_RetryChildrenHaveNamespacedStepRef(t *testing.T) {
	// Simulates the pipeline: ApplyRetries runs on children BEFORE ApplyRalph,
	// so children arrive with retry-expanded step_refs that need re-namespacing.
	retryChildren := []*Step{
		{
			ID:    "review",
			Title: "Review",
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
		{
			ID:    "apply",
			Title: "Apply",
			Needs: []string{"review"},
		},
	}

	// Stage 10: expand retries on children
	expandedChildren, err := ApplyRetries(retryChildren)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}

	// Stage 11: wrap in ralph
	steps := []*Step{
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
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	// Find all body steps (skip control + iteration scope)
	for _, step := range got {
		if step.ID == "review-loop" || step.ID == "review-loop.iteration.1" || isSourceSpecStep(step) {
			continue
		}
		ref := step.Metadata["gc.step_ref"]
		if ref != step.ID {
			t.Errorf("step %q: gc.step_ref = %q, want %q (should be namespaced)", step.ID, ref, step.ID)
		}
	}

	// Specifically check the retry attempt — this is the bug case.
	// The attempt was created by expandRetry with gc.step_ref = "review.attempt.1"
	// but after ralph namespacing it should be "review-loop.iteration.1.review.attempt.1"
	var foundAttempt bool
	for _, step := range got {
		if step.ID == "review-loop.iteration.1.review.attempt.1" {
			foundAttempt = true
			ref := step.Metadata["gc.step_ref"]
			if ref != "review-loop.iteration.1.review.attempt.1" {
				t.Errorf("retry attempt gc.step_ref = %q, want %q", ref, "review-loop.iteration.1.review.attempt.1")
			}
		}
	}
	if !foundAttempt {
		ids := make([]string, len(got))
		for i, s := range got {
			ids[i] = s.ID
		}
		t.Errorf("retry attempt step not found; steps: %v", ids)
	}
}

func TestApplyRalph_ComposeExpandChildrenHaveNamespacedStepRef(t *testing.T) {
	// Simulates compose.expand producing multi-segment child IDs
	// like "review-pipeline.review-claude". These children also have retry.
	// After ApplyRetries + ApplyRalph, all step_refs must be fully namespaced.
	retryChildren := []*Step{
		{
			ID:    "review-pipeline.review-claude",
			Title: "Code review: Claude",
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
		{
			ID:    "review-pipeline.review-codex",
			Title: "Code review: Codex",
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
		{
			ID:    "review-pipeline.synthesize",
			Title: "Synthesize",
			Needs: []string{"review-pipeline.review-claude", "review-pipeline.review-codex"},
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
		{
			ID:    "apply-fixes",
			Title: "Apply fixes",
			Needs: []string{"review-pipeline.synthesize"},
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
	}

	// Stage 10: expand retries
	expandedChildren, err := ApplyRetries(retryChildren)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}

	// Stage 11: wrap in ralph
	steps := []*Step{
		{
			ID:    "review-loop",
			Title: "Review loop",
			Type:  "task",
			Ralph: &RalphSpec{
				MaxAttempts: 999,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: expandedChildren,
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	// Every body step must have gc.step_ref == step.ID (fully namespaced)
	var mismatches []string
	for _, step := range got {
		if step.ID == "review-loop" {
			continue // control doesn't need this check
		}
		if isSourceSpecStep(step) {
			continue
		}
		ref := step.Metadata["gc.step_ref"]
		if ref != step.ID {
			mismatches = append(mismatches, step.ID+": got "+ref)
		}
	}
	if len(mismatches) > 0 {
		t.Errorf("step_ref mismatches (gc.step_ref != step.ID):\n")
		for _, m := range mismatches {
			t.Errorf("  %s", m)
		}
	}

	// Verify specific compose.expand attempt beads exist with correct refs
	expectedSteps := []string{
		"review-loop.iteration.1.review-pipeline.review-claude",
		"review-loop.iteration.1.review-pipeline.review-claude.attempt.1",
		"review-loop.iteration.1.review-pipeline.review-codex",
		"review-loop.iteration.1.review-pipeline.review-codex.attempt.1",
		"review-loop.iteration.1.review-pipeline.synthesize",
		"review-loop.iteration.1.review-pipeline.synthesize.attempt.1",
		"review-loop.iteration.1.apply-fixes",
		"review-loop.iteration.1.apply-fixes.attempt.1",
	}
	stepIDs := make(map[string]bool, len(got))
	for _, s := range got {
		stepIDs[s.ID] = true
	}
	for _, expected := range expectedSteps {
		if !stepIDs[expected] {
			t.Errorf("missing expected step %q", expected)
		}
	}
}

func TestApplyRalph_NestedRetryInsideRalphStepRefChains(t *testing.T) {
	// Test that nested retry inside ralph has fully-qualified step_refs
	// at every level of nesting.
	children := []*Step{
		{
			ID:    "work",
			Title: "Do work",
			Retry: &RetrySpec{MaxAttempts: 2, OnExhausted: "hard_fail"},
		},
	}

	expanded, err := ApplyRetries(children)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}

	steps := []*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{
				MaxAttempts: 5,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: expanded,
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	// Check that the retry control has namespaced step_ref.
	foundSpec := false
	for _, step := range got {
		if step.ID == "outer.iteration.1.work" {
			ref := step.Metadata["gc.step_ref"]
			if ref != "outer.iteration.1.work" {
				t.Errorf("retry control gc.step_ref = %q, want %q", ref, "outer.iteration.1.work")
			}
			if step.Metadata["gc.source_step_spec"] != "" {
				t.Errorf("retry control gc.source_step_spec = %q, want empty", step.Metadata["gc.source_step_spec"])
			}
		}
		if step.ID == "outer.iteration.1.work.spec" {
			foundSpec = true
			assertFrozenSpecStep(t, step, "work", nil)
		}
		if step.ID == "outer.iteration.1.work.attempt.1" {
			ref := step.Metadata["gc.step_ref"]
			if ref != "outer.iteration.1.work.attempt.1" {
				t.Errorf("retry attempt gc.step_ref = %q, want %q", ref, "outer.iteration.1.work.attempt.1")
			}
		}
	}
	if !foundSpec {
		t.Fatal("missing retry control spec bead")
	}
}

func TestApplyRalph_NestedRetryControlsPreserveOwnStepID(t *testing.T) {
	// Nested retry controls inside a ralph must keep their OWN step_id,
	// not inherit the ralph owner's. Otherwise find_canonical_control
	// collapses all nested controls into the ralph node.
	retryChildren := []*Step{
		{
			ID:    "review-claude",
			Title: "Claude review",
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
		{
			ID:    "review-codex",
			Title: "Codex review",
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
		{
			ID:    "synthesize",
			Title: "Synthesize",
			Needs: []string{"review-claude", "review-codex"},
			Retry: &RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
		},
	}

	expanded, err := ApplyRetries(retryChildren)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}

	steps := []*Step{
		{
			ID:    "review-loop",
			Title: "Review loop",
			Ralph: &RalphSpec{
				MaxAttempts: 999,
				Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
			},
			Children: expanded,
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	// Each retry control inside the ralph should have its OWN step_id,
	// not the ralph owner's "review-loop".
	controlStepIDs := map[string]string{
		"review-loop.iteration.1.review-claude": "",
		"review-loop.iteration.1.review-codex":  "",
		"review-loop.iteration.1.synthesize":    "",
	}
	for _, step := range got {
		if _, want := controlStepIDs[step.ID]; want {
			controlStepIDs[step.ID] = step.Metadata["gc.step_id"]
		}
	}

	for stepID, gotStepID := range controlStepIDs {
		if gotStepID == "review-loop" {
			t.Errorf("step %q has gc.step_id=%q (inherited from ralph owner), should have its own",
				stepID, gotStepID)
		}
		if gotStepID == "" {
			t.Errorf("step %q not found in output", stepID)
		}
	}

	// Verify they're all DIFFERENT from each other
	seen := map[string]string{}
	for stepID, gotStepID := range controlStepIDs {
		if prev, dup := seen[gotStepID]; dup {
			t.Errorf("step %q and %q share gc.step_id=%q — find_canonical_control will collapse them",
				stepID, prev, gotStepID)
		}
		seen[gotStepID] = stepID
	}
}

func TestApplyRalph_PreservesNonRalphSteps(t *testing.T) {
	steps := []*Step{
		{ID: "setup", Title: "Setup"},
		{ID: "work", Title: "Work", Ralph: &RalphSpec{
			MaxAttempts: 2,
			Check:       &RalphCheckSpec{Mode: "exec", Path: "check.sh"},
		}},
		{ID: "cleanup", Title: "Cleanup"},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	// setup + (control + spec + iteration) + cleanup = 5
	if len(got) != 5 {
		t.Fatalf("len(got) = %d, want 5", len(got))
	}
	if got[0].ID != "setup" {
		t.Errorf("got[0].ID = %q, want setup", got[0].ID)
	}
	if got[1].ID != "work" { // control
		t.Errorf("got[1].ID = %q, want work (control)", got[1].ID)
	}
	if got[2].ID != "work.spec" {
		t.Errorf("got[2].ID = %q, want work.spec", got[2].ID)
	}
	if got[3].ID != "work.iteration.1" {
		t.Errorf("got[3].ID = %q, want work.iteration.1", got[3].ID)
	}
	if got[4].ID != "cleanup" {
		t.Errorf("got[4].ID = %q, want cleanup", got[4].ID)
	}
}
