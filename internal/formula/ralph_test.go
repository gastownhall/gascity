package formula

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

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

func TestApplyRalph_StepTimeoutPropagated(t *testing.T) {
	steps := []*Step{
		{
			ID:      "build",
			Title:   "Build project",
			Type:    "task",
			Timeout: "10m",
			Ralph: &RalphSpec{
				MaxAttempts: 3,
				Check: &RalphCheckSpec{
					Mode: "exec",
					Path: "scripts/check.sh",
				},
			},
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	control := got[0]
	if control.Metadata["gc.step_timeout"] != "10m" {
		t.Errorf("control gc.step_timeout = %q, want 10m", control.Metadata["gc.step_timeout"])
	}
}

func TestApplyRalph_StepTimeoutOmittedWhenEmpty(t *testing.T) {
	steps := []*Step{
		{
			ID:    "build",
			Title: "Build project",
			Type:  "task",
			// No Timeout set
			Ralph: &RalphSpec{
				MaxAttempts: 3,
				Check: &RalphCheckSpec{
					Mode: "exec",
					Path: "scripts/check.sh",
				},
			},
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}

	control := got[0]
	if _, exists := control.Metadata["gc.step_timeout"]; exists {
		t.Errorf("control should not have gc.step_timeout when step.Timeout is empty, got %q", control.Metadata["gc.step_timeout"])
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

// TestMarkRalphBodyOutputSinksTracksBeadmetaExemptKinds keeps the ralph
// body-sink marker in lockstep with beadmeta.ScopeCheckExemptKinds plus its
// one deliberate extra exclusion, KindRalph (a nested ralph control's output
// contract is owned by its own OnComplete, not by the enclosing body). Before
// ga-e154xo this list lagged by {tally, drain}, so hand-written drain/tally
// control steps that were body sinks were wrongly marked
// gc.output_json_required even though control beads are never worker-executed.
func TestMarkRalphBodyOutputSinksTracksBeadmetaExemptKinds(t *testing.T) {
	exempt := append([]string{beadmeta.KindRalph}, beadmeta.ScopeCheckExemptKinds...)
	for _, kind := range exempt {
		steps := []*Step{
			{
				ID:       "sink",
				Title:    "Sink",
				Metadata: map[string]string{beadmeta.KindMetadataKey: kind},
			},
		}
		markRalphBodyOutputSinks(steps)
		if got := steps[0].Metadata[beadmeta.OutputJSONRequiredMetadataKey]; got == "true" {
			t.Errorf("markRalphBodyOutputSinks marked kind=%q sink, want skipped", kind)
		}
	}

	for _, kind := range []string{"", beadmeta.KindTask, beadmeta.KindRetry} {
		steps := []*Step{
			{
				ID:       "sink",
				Title:    "Sink",
				Metadata: map[string]string{beadmeta.KindMetadataKey: kind},
			},
		}
		markRalphBodyOutputSinks(steps)
		if got := steps[0].Metadata[beadmeta.OutputJSONRequiredMetadataKey]; got != "true" {
			t.Errorf("markRalphBodyOutputSinks did not mark kind=%q sink, want marked", kind)
		}
	}

	// Referenced (non-sink) steps and teardown-role steps stay unmarked
	// regardless of kind.
	steps := []*Step{
		{ID: "upstream", Title: "Upstream"},
		{ID: "downstream", Title: "Downstream", Needs: []string{"upstream"}},
		{
			ID:    "teardown",
			Title: "Teardown",
			Metadata: map[string]string{
				beadmeta.ScopeRoleMetadataKey: beadmeta.ScopeRoleTeardown,
			},
		},
	}
	markRalphBodyOutputSinks(steps)
	if steps[0].Metadata[beadmeta.OutputJSONRequiredMetadataKey] == "true" {
		t.Error("referenced upstream step was marked as an output sink")
	}
	if steps[1].Metadata[beadmeta.OutputJSONRequiredMetadataKey] != "true" {
		t.Error("sink downstream step was not marked as an output sink")
	}
	if steps[2].Metadata[beadmeta.OutputJSONRequiredMetadataKey] == "true" {
		t.Error("teardown-role step was marked as an output sink")
	}
}

func TestApplyRalph_StampsControlForOnIterationRoot(t *testing.T) {
	// Simple ralph (no children): iteration.1 work bead carries the stamp.
	simple := []*Step{
		{
			ID:    "implement",
			Title: "Implement",
			Type:  "task",
			Ralph: &RalphSpec{MaxAttempts: 3, Check: &RalphCheckSpec{Mode: "exec", Path: "c.sh"}},
		},
	}
	got, err := ApplyRalph(simple)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	control, iteration := got[0], got[2]
	if iteration.Metadata[beadmeta.ControlForMetadataKey] != "implement" {
		t.Fatalf("simple iteration gc.control_for = %q, want implement", iteration.Metadata[beadmeta.ControlForMetadataKey])
	}
	if control.Metadata[beadmeta.StepIDMetadataKey] != "implement" {
		t.Fatalf("control gc.step_id = %q, want implement (must match iteration gc.control_for)", control.Metadata[beadmeta.StepIDMetadataKey])
	}
	if _, ok := control.Metadata[beadmeta.ControlForMetadataKey]; ok {
		t.Fatalf("control must not carry gc.control_for")
	}

	// Nested ralph: only the iteration scope root carries the stamp; body
	// children (which are not attempt roots) must not.
	nested := []*Step{
		{
			ID:    "review-loop",
			Title: "Review loop",
			Type:  "task",
			Ralph: &RalphSpec{MaxAttempts: 3, Check: &RalphCheckSpec{Mode: "exec", Path: "c.sh"}},
			Children: []*Step{
				{ID: "review", Title: "Review"},
				{ID: "apply", Title: "Apply", Needs: []string{"review"}},
			},
		},
	}
	got2, err := ApplyRalph(nested)
	if err != nil {
		t.Fatalf("ApplyRalph nested failed: %v", err)
	}
	scope, reviewChild, applyChild := got2[2], got2[3], got2[4]
	if scope.Metadata[beadmeta.ControlForMetadataKey] != "review-loop" {
		t.Fatalf("nested scope gc.control_for = %q, want review-loop", scope.Metadata[beadmeta.ControlForMetadataKey])
	}
	if _, ok := reviewChild.Metadata[beadmeta.ControlForMetadataKey]; ok {
		t.Fatalf("body child %q must not carry gc.control_for", reviewChild.ID)
	}
	if _, ok := applyChild.Metadata[beadmeta.ControlForMetadataKey]; ok {
		t.Fatalf("body child %q must not carry gc.control_for", applyChild.ID)
	}
}

// TestApplyRalph_NamespacesNestedControlForAcrossBody is the S38 nested-lineage
// regression guard for the producer side. An outer ralph with a retry child
// must stamp the nested retry's attempt root with the *namespaced* control ref
// (the cloned inner control's gc.step_ref), not the bare inner step id.
//
// The bare step id ("inner") is shared by the inner control of every sibling
// outer ralph iteration, so a bare gc.control_for let findLatestAttempt's
// primary lookup match a foreign iteration's inner control through the shared
// gc.step_id identity member. Namespacing the stamp scopes it to this outer
// iteration's inner control, mirroring the runtime buildNestedControlSeed path.
func TestApplyRalph_NamespacesNestedControlForAcrossBody(t *testing.T) {
	// Mirror the compile pipeline order: retries expand before ralph, so the
	// ralph body already holds the inner control + attempt beads when
	// namespaceRalphBodySteps runs over them.
	steps := []*Step{
		{
			ID:    "review-loop",
			Title: "Review loop",
			Type:  "task",
			Ralph: &RalphSpec{MaxAttempts: 3, Check: &RalphCheckSpec{Mode: "exec", Path: "c.sh"}},
			Children: []*Step{
				{
					ID:    "inner",
					Title: "Inner",
					Retry: &RetrySpec{MaxAttempts: 2},
				},
			},
		},
	}
	retried, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries: %v", err)
	}
	expanded, err := ApplyRalph(retried)
	if err != nil {
		t.Fatalf("ApplyRalph: %v", err)
	}

	var innerControl, innerAttempt *Step
	for _, s := range expanded {
		switch {
		case s.ID == "review-loop.iteration.1.inner" && s.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRetry:
			innerControl = s
		case s.ID == "review-loop.iteration.1.inner.attempt.1":
			innerAttempt = s
		}
	}
	if innerControl == nil {
		t.Fatalf("nested inner control not found among expanded steps")
	}
	if innerAttempt == nil {
		t.Fatalf("nested inner attempt root not found among expanded steps")
	}

	wantCF := innerControl.Metadata[beadmeta.StepRefMetadataKey]
	if wantCF == "" || wantCF == "inner" {
		t.Fatalf("inner control gc.step_ref = %q, want a namespaced ref", wantCF)
	}
	got := innerAttempt.Metadata[beadmeta.ControlForMetadataKey]
	if got == "inner" {
		t.Fatalf("nested attempt gc.control_for is the bare step id %q; must be namespaced to the inner control ref", got)
	}
	if got != wantCF {
		t.Fatalf("nested attempt gc.control_for = %q, want %q (must equal inner control gc.step_ref)", got, wantCF)
	}
}

func TestApplyRalph_RejectsUnresolvedMaxAttemptsVar(t *testing.T) {
	steps := []*Step{{
		ID:    "implement",
		Title: "Implement",
		Ralph: &RalphSpec{MaxAttemptsVar: "attempts", Check: &RalphCheckSpec{Mode: "exec", Path: "check.sh"}},
	}}

	if _, err := ApplyRalph(steps); err == nil || !strings.Contains(err.Error(), `max_attempts var "attempts" is unresolved`) {
		t.Fatalf("ApplyRalph error = %v, want an unresolved max_attempts var error", err)
	}
}

func TestBindCheckMaxAttemptsVars_KeepsFirstBinding(t *testing.T) {
	steps := []*Step{{
		ID:    "phase",
		Title: "Phase",
		Children: []*Step{{
			ID:    "implement",
			Title: "Implement",
			Ralph: &RalphSpec{MaxAttemptsVar: "attempts", Check: &RalphCheckSpec{Mode: "exec", Path: "check.sh"}},
		}},
	}}

	bindCheckMaxAttemptsVars(steps, map[string]string{"attempts": "2"})
	bindCheckMaxAttemptsVars(steps, map[string]string{"attempts": "9"})
	if err := ResolveCheckMaxAttemptsVars(steps, map[string]string{"attempts": "5"}); err != nil {
		t.Fatalf("ResolveCheckMaxAttemptsVars: %v", err)
	}
	if got := steps[0].Children[0].Ralph; got.MaxAttempts != 2 || got.MaxAttemptsVar != "" {
		t.Fatalf("resolved check = %+v, want MaxAttempts 2 from the first binding", got)
	}
}

// TestApplyRalph_NestedRalphWithChildrenKeepsInnerScope pins issue #9412: a
// check loop with children nested in another check loop's body must leave the
// inner iteration scope and its members scoped to the inner loop, so the inner
// member's scope-check evaluates the inner iteration, not the outer one.
func TestApplyRalph_NestedRalphWithChildrenKeepsInnerScope(t *testing.T) {
	steps := []*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{
				{
					ID:       "inner",
					Title:    "Inner loop",
					Ralph:    &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
					Children: []*Step{{ID: "i1", Title: "Inner work"}},
				},
				{ID: "o1", Title: "Outer work", Needs: []string{"inner"}},
			},
		},
	}

	got, err := ApplyRalph(steps)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	byID := make(map[string]*Step, len(got))
	for _, s := range got {
		byID[s.ID] = s
	}

	const innerIteration = "outer.iteration.1.inner.iteration.1"
	member := byID[innerIteration+".i1"]
	if member == nil {
		t.Fatalf("missing inner member %s.i1", innerIteration)
	}
	if ref := member.Metadata[beadmeta.ScopeRefMetadataKey]; ref != innerIteration {
		t.Errorf("inner member gc.scope_ref = %q, want %q", ref, innerIteration)
	}
	if owner := member.Metadata[beadmeta.RalphStepIDMetadataKey]; owner != "inner" {
		t.Errorf("inner member gc.ralph_step_id = %q, want %q", owner, "inner")
	}

	scope := byID[innerIteration]
	if scope == nil {
		t.Fatalf("missing inner iteration scope %s", innerIteration)
	}
	if ref := scope.Metadata[beadmeta.ScopeRefMetadataKey]; ref == "outer.iteration.1" {
		t.Errorf("inner iteration scope gc.scope_ref = %q: inner scope joined the outer iteration as a member", ref)
	}
}

// nestedCheckSteps builds the nested-check shape of issue #9412 at one nesting
// depth: an outer check loop whose body holds an inner check loop (children
// i1) and o1. With o1NeedsInner, o1 waits on the inner control; otherwise o1
// runs parallel to the inner loop.
func nestedCheckSteps(o1NeedsInner bool) []*Step {
	o1 := &Step{ID: "o1", Title: "Outer work after the inner loop"}
	if o1NeedsInner {
		o1.Needs = []string{"inner"}
	}
	return []*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{
				{
					ID:       "inner",
					Title:    "Inner loop",
					Ralph:    &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
					Children: []*Step{{ID: "i1", Title: "Inner work"}},
				},
				o1,
			},
		},
	}
}

// threeLevelNestedCheckSteps nests a check three levels deep: outer > mid >
// inner, with one work member at the innermost level.
func threeLevelNestedCheckSteps() []*Step {
	innermost := &Step{
		ID:       "inner",
		Title:    "Innermost loop",
		Ralph:    &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
		Children: []*Step{{ID: "i1", Title: "Innermost work"}},
	}
	mid := &Step{
		ID:       "mid",
		Title:    "Mid loop",
		Ralph:    &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
		Children: []*Step{innermost},
	}
	return []*Step{
		{
			ID:       "outer",
			Title:    "Outer loop",
			Ralph:    &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{mid},
		},
	}
}

// nestedCheckWithRetrySteps adds a retry-managed step to the inner loop's
// body, alongside the plain member i1.
func nestedCheckWithRetrySteps() []*Step {
	return []*Step{
		{
			ID:    "outer",
			Title: "Outer loop",
			Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
			Children: []*Step{
				{
					ID:    "inner",
					Title: "Inner loop",
					Ralph: &RalphSpec{MaxAttempts: 2, Check: &RalphCheckSpec{Mode: "exec", Path: "scripts/pass.sh"}},
					Children: []*Step{
						{ID: "i1", Title: "Inner work"},
						{ID: "r1", Title: "Retry work", Retry: &RetrySpec{MaxAttempts: 2}},
					},
				},
			},
		},
	}
}

// compileNestedCheck expands steps the way the v2 compiler does (retries, then
// ralph) and applies graph controls, returning every step keyed by ID.
func compileNestedCheck(t *testing.T, steps []*Step) map[string]*Step {
	t.Helper()
	expanded, err := ApplyRetries(steps)
	if err != nil {
		t.Fatalf("ApplyRetries failed: %v", err)
	}
	expanded, err = ApplyRalph(expanded)
	if err != nil {
		t.Fatalf("ApplyRalph failed: %v", err)
	}
	f := &Formula{Steps: expanded}
	ApplyGraphControls(f)
	byID := make(map[string]*Step)
	for _, s := range collectGraphSteps(f.Steps) {
		byID[s.ID] = s
	}
	return byID
}

// assertInnerLoopIdentity checks the inner loop's compile contract for the
// nested-check shape at one level (issue #9412, AC-1): the inner member and its
// scope-check stay scoped to the inner iteration, the inner iteration scope
// keeps its own loop identity and edges, and only the inner control joins the
// outer iteration.
func assertInnerLoopIdentity(t *testing.T, byID map[string]*Step) {
	t.Helper()
	const innerIteration = "outer.iteration.1.inner.iteration.1"

	member := byID[innerIteration+".i1"]
	if member == nil {
		t.Fatalf("missing inner member %s.i1", innerIteration)
	}
	wantMember := map[string]string{
		beadmeta.ScopeRefMetadataKey:    innerIteration,
		beadmeta.RalphStepIDMetadataKey: "inner",
		beadmeta.ScopeRoleMetadataKey:   beadmeta.ScopeRoleMember,
		beadmeta.OnFailMetadataKey:      "abort_scope",
		beadmeta.AttemptMetadataKey:     "1",
		beadmeta.IterationMetadataKey:   "1",
	}
	for key, want := range wantMember {
		if got := member.Metadata[key]; got != want {
			t.Errorf("inner member %s = %q, want %q", key, got, want)
		}
	}

	scopeCheck := byID[innerIteration+".i1-scope-check"]
	if scopeCheck == nil {
		t.Fatalf("missing minted %s.i1-scope-check", innerIteration)
	}
	if got := scopeCheck.Metadata[beadmeta.KindMetadataKey]; got != beadmeta.KindScopeCheck {
		t.Errorf("inner member scope-check gc.kind = %q, want %q", got, beadmeta.KindScopeCheck)
	}
	for _, key := range []string{beadmeta.ScopeRefMetadataKey, beadmeta.RalphStepIDMetadataKey} {
		if got := scopeCheck.Metadata[key]; got != member.Metadata[key] {
			t.Errorf("inner member scope-check %s = %q, want %q (same as the member)", key, got, member.Metadata[key])
		}
	}

	scope := byID[innerIteration]
	if scope == nil {
		t.Fatalf("missing inner iteration scope %s", innerIteration)
	}
	wantScope := map[string]string{
		beadmeta.KindMetadataKey:        beadmeta.KindScope,
		beadmeta.ScopeRoleMetadataKey:   beadmeta.ScopeRoleBody,
		beadmeta.ScopeNameMetadataKey:   "inner",
		beadmeta.StepIDMetadataKey:      "inner",
		beadmeta.RalphStepIDMetadataKey: "inner",
		beadmeta.ControlForMetadataKey:  "outer.iteration.1.inner",
	}
	for key, want := range wantScope {
		if got := scope.Metadata[key]; got != want {
			t.Errorf("inner iteration scope %s = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{beadmeta.ScopeRefMetadataKey, beadmeta.OnFailMetadataKey} {
		if got, ok := scope.Metadata[key]; ok && got != "" {
			t.Errorf("inner iteration scope must not carry %s, got %q", key, got)
		}
	}
	if !containsString(scope.Needs, innerIteration+".i1") {
		t.Errorf("inner iteration scope needs = %v, want %q directly (the ga-a6zy9 guard keeps the edge raw)", scope.Needs, innerIteration+".i1")
	}
	if containsString(scope.Needs, innerIteration+".i1-scope-check") {
		t.Errorf("inner iteration scope needs = %v, must not block on its own closer %q", scope.Needs, innerIteration+".i1-scope-check")
	}

	control := byID["outer.iteration.1.inner"]
	if control == nil {
		t.Fatal("missing inner control outer.iteration.1.inner")
	}
	wantControl := map[string]string{
		beadmeta.KindMetadataKey:        beadmeta.KindRalph,
		beadmeta.StepIDMetadataKey:      "inner",
		beadmeta.ScopeRefMetadataKey:    "outer.iteration.1",
		beadmeta.ScopeRoleMetadataKey:   beadmeta.ScopeRoleMember,
		beadmeta.OnFailMetadataKey:      "abort_scope",
		beadmeta.RalphStepIDMetadataKey: "outer",
	}
	for key, want := range wantControl {
		if got := control.Metadata[key]; got != want {
			t.Errorf("inner control %s = %q, want %q", key, got, want)
		}
	}
}

// TestApplyRalph_NestedCheckWithChildrenKeepsInnerLoopIdentity pins the
// compile contract of issue #9412 across shapes: a check-with-children nested
// in a check body keeps the inner loop's identity while only the inner
// control joins the outer iteration.
func TestApplyRalph_NestedCheckWithChildrenKeepsInnerLoopIdentity(t *testing.T) {
	t.Run("two_level", func(t *testing.T) {
		byID := compileNestedCheck(t, nestedCheckSteps(true))
		assertInnerLoopIdentity(t, byID)

		// o1's need on the inner control gates on the control's minted
		// scope-check, exactly as for any nested control today.
		o1 := byID["outer.iteration.1.o1"]
		if o1 == nil {
			t.Fatal("missing outer member outer.iteration.1.o1")
		}
		if len(o1.Needs) != 1 || o1.Needs[0] != "outer.iteration.1.inner-scope-check" {
			t.Errorf("o1 needs = %v, want [outer.iteration.1.inner-scope-check]", o1.Needs)
		}
	})

	t.Run("o1_parallel", func(t *testing.T) {
		byID := compileNestedCheck(t, nestedCheckSteps(false))
		assertInnerLoopIdentity(t, byID)
		o1 := byID["outer.iteration.1.o1"]
		if o1 == nil {
			t.Fatal("missing outer member outer.iteration.1.o1")
		}
		if len(o1.Needs) != 0 {
			t.Errorf("parallel o1 needs = %v, want none", o1.Needs)
		}
	})

	t.Run("three_level", func(t *testing.T) {
		byID := compileNestedCheck(t, threeLevelNestedCheckSteps())

		const midIteration = "outer.iteration.1.mid.iteration.1"
		const innerIteration = midIteration + ".inner.iteration.1"

		i1 := byID[innerIteration+".i1"]
		if i1 == nil {
			t.Fatalf("missing innermost member %s.i1", innerIteration)
		}
		if got := i1.Metadata[beadmeta.ScopeRefMetadataKey]; got != innerIteration {
			t.Errorf("innermost i1 gc.scope_ref = %q, want %q", got, innerIteration)
		}
		if got := i1.Metadata[beadmeta.RalphStepIDMetadataKey]; got != "inner" {
			t.Errorf("innermost i1 gc.ralph_step_id = %q, want inner", got)
		}

		midScope := byID[midIteration]
		if midScope == nil {
			t.Fatalf("missing mid iteration scope %s", midIteration)
		}
		if got, ok := midScope.Metadata[beadmeta.ScopeRefMetadataKey]; ok && got != "" {
			t.Errorf("mid iteration scope gc.scope_ref = %q, want none", got)
		}
		if got := midScope.Metadata[beadmeta.RalphStepIDMetadataKey]; got != "mid" {
			t.Errorf("mid iteration scope gc.ralph_step_id = %q, want mid", got)
		}

		innerControl := byID[midIteration+".inner"]
		if innerControl == nil {
			t.Fatalf("missing innermost control %s.inner", midIteration)
		}
		if got := innerControl.Metadata[beadmeta.ScopeRefMetadataKey]; got != midIteration {
			t.Errorf("innermost control gc.scope_ref = %q, want %q", got, midIteration)
		}
		if got := innerControl.Metadata[beadmeta.RalphStepIDMetadataKey]; got != "mid" {
			t.Errorf("innermost control gc.ralph_step_id = %q, want mid", got)
		}

		innerScope := byID[innerIteration]
		if innerScope == nil {
			t.Fatalf("missing innermost iteration scope %s", innerIteration)
		}
		if got, ok := innerScope.Metadata[beadmeta.ScopeRefMetadataKey]; ok && got != "" {
			t.Errorf("innermost iteration scope gc.scope_ref = %q, want none", got)
		}
		if got := innerScope.Metadata[beadmeta.RalphStepIDMetadataKey]; got != "inner" {
			t.Errorf("innermost iteration scope gc.ralph_step_id = %q, want inner", got)
		}
		if got := innerScope.Metadata[beadmeta.ControlForMetadataKey]; got != midIteration+".inner" {
			t.Errorf("innermost iteration scope gc.control_for = %q, want %q", got, midIteration+".inner")
		}
	})

	t.Run("retry_in_inner_body", func(t *testing.T) {
		byID := compileNestedCheck(t, nestedCheckWithRetrySteps())
		assertInnerLoopIdentity(t, byID)

		const innerIteration = "outer.iteration.1.inner.iteration.1"
		retryControl := byID[innerIteration+".r1"]
		if retryControl == nil {
			t.Fatalf("missing retry control %s.r1", innerIteration)
		}
		if got := retryControl.Metadata[beadmeta.KindMetadataKey]; got != beadmeta.KindRetry {
			t.Errorf("retry control gc.kind = %q, want %q", got, beadmeta.KindRetry)
		}
		if got := retryControl.Metadata[beadmeta.ScopeRefMetadataKey]; got != innerIteration {
			t.Errorf("retry control gc.scope_ref = %q, want %q", got, innerIteration)
		}
		if got := retryControl.Metadata[beadmeta.RalphStepIDMetadataKey]; got != "inner" {
			t.Errorf("retry control gc.ralph_step_id = %q, want inner", got)
		}
		if got := retryControl.Metadata[beadmeta.AttemptMetadataKey]; got != "1" {
			t.Errorf("retry control gc.attempt = %q, want 1 (the inner iteration)", got)
		}

		retryAttempt := byID[innerIteration+".r1.attempt.1"]
		if retryAttempt == nil {
			t.Fatalf("missing retry attempt %s.r1.attempt.1", innerIteration)
		}
		wantAttempt := map[string]string{
			beadmeta.ScopeRefMetadataKey:     innerIteration,
			beadmeta.RalphStepIDMetadataKey:  "inner",
			beadmeta.RetryAttemptMetadataKey: "1",
			beadmeta.AttemptMetadataKey:      "1",
			beadmeta.ControlForMetadataKey:   innerIteration + ".r1",
		}
		for key, want := range wantAttempt {
			if got := retryAttempt.Metadata[key]; got != want {
				t.Errorf("retry attempt %s = %q, want %q", key, got, want)
			}
		}
	})
}
