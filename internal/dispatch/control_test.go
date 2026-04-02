package dispatch

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// ---------------------------------------------------------------------------
// processRetryControl tests
// ---------------------------------------------------------------------------

func TestProcessRetryControlPass(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review",
			"gc.step_id":          "review",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": `{"id":"review","title":"Review","type":"task","retry":{"max_attempts":3}}`,
			"gc.control_epoch":    "1",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "review attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review.attempt.1",
			"gc.attempt":      "1",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"ok":true}`,
			"review.verdict":  "approved",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	after := mustGet(t, store, control.ID)
	if after.Status != "closed" {
		t.Fatalf("control status = %q, want closed", after.Status)
	}
	if after.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("control outcome = %q, want pass", after.Metadata["gc.outcome"])
	}
	if after.Metadata["gc.output_json"] != `{"ok":true}` {
		t.Fatalf("control output_json = %q, want propagated", after.Metadata["gc.output_json"])
	}
	if after.Metadata["review.verdict"] != "approved" {
		t.Fatalf("control review.verdict = %q, want approved", after.Metadata["review.verdict"])
	}
}

func TestProcessRetryControlHardFail(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review",
			"gc.step_id":          "review",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": `{"id":"review","title":"Review","type":"task","retry":{"max_attempts":3}}`,
			"gc.control_epoch":    "1",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "review attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.step_ref":       "mol-test.review.attempt.1",
			"gc.attempt":        "1",
			"gc.outcome":        "fail",
			"gc.failure_class":  "hard",
			"gc.failure_reason": "auth_error",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if result.Action != "hard-fail" {
		t.Fatalf("action = %q, want hard-fail", result.Action)
	}

	after := mustGet(t, store, control.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("control = status %q outcome %q, want closed/fail", after.Status, after.Metadata["gc.outcome"])
	}
	if after.Metadata["gc.final_disposition"] != "hard_fail" {
		t.Fatalf("disposition = %q, want hard_fail", after.Metadata["gc.final_disposition"])
	}
}

func TestProcessRetryControlTransientRetry(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review",
			"gc.step_id":          "review",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": `{"id":"review","title":"Review","type":"task","retry":{"max_attempts":3}}`,
			"gc.control_epoch":    "1",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "review attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.step_ref":       "mol-test.review.attempt.1",
			"gc.attempt":        "1",
			"gc.outcome":        "fail",
			"gc.failure_class":  "transient",
			"gc.failure_reason": "rate_limited",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if result.Action != "retry" {
		t.Fatalf("action = %q, want retry", result.Action)
	}

	// Control bead should still be open (waiting on attempt 2).
	after := mustGet(t, store, control.ID)
	if after.Status != "open" {
		t.Fatalf("control status = %q, want open (blocking on attempt 2)", after.Status)
	}

	// Should have a new blocking dep (attempt 2).
	deps, _ := store.DepList(control.ID, "down")
	if len(deps) < 2 {
		t.Fatalf("control deps = %d, want >= 2 (attempt 1 + attempt 2)", len(deps))
	}

	// Epoch should have advanced.
	if after.Metadata["gc.control_epoch"] != "2" {
		t.Fatalf("epoch = %q, want 2", after.Metadata["gc.control_epoch"])
	}

	// Attempt log should record the decision.
	var log []map[string]string
	if err := json.Unmarshal([]byte(after.Metadata["gc.attempt_log"]), &log); err != nil {
		t.Fatalf("unmarshal attempt_log: %v", err)
	}
	if len(log) != 1 || log[0]["outcome"] != "transient" {
		t.Fatalf("attempt_log = %v, want [{attempt:1 outcome:transient}]", log)
	}
}

func TestProcessRetryControlSoftFailOnExhaustion(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review",
			"gc.step_id":          "review",
			"gc.max_attempts":     "1",
			"gc.on_exhausted":     "soft_fail",
			"gc.source_step_spec": `{"id":"review","title":"Review","type":"task","retry":{"max_attempts":1,"on_exhausted":"soft_fail"}}`,
			"gc.control_epoch":    "1",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "review attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.step_ref":       "mol-test.review.attempt.1",
			"gc.attempt":        "1",
			"gc.outcome":        "fail",
			"gc.failure_class":  "transient",
			"gc.failure_reason": "rate_limited",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if result.Action != "soft-fail" {
		t.Fatalf("action = %q, want soft-fail", result.Action)
	}

	after := mustGet(t, store, control.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("control = status %q outcome %q, want closed/pass (soft-fail closes as pass)", after.Status, after.Metadata["gc.outcome"])
	}
	if after.Metadata["gc.final_disposition"] != "soft_fail" {
		t.Fatalf("disposition = %q, want soft_fail", after.Metadata["gc.final_disposition"])
	}
}

func TestProcessRetryControlInvariantViolation(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review",
			"gc.step_id":          "review",
			"gc.max_attempts":     "3",
			"gc.source_step_spec": `{"id":"review","title":"Review","type":"task"}`,
			"gc.control_epoch":    "1",
		},
	})
	// Attempt is still open -- control should not be processing.
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "review attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review.attempt.1",
			"gc.attempt":      "1",
		},
	})
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	_, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err == nil {
		t.Fatal("expected invariant violation error")
	}
	if !strings.Contains(err.Error(), "invariant violation") {
		t.Fatalf("error = %v, want invariant violation", err)
	}
}

func TestProcessRetryControlControllerError(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	// Control with bad source_step_spec (invalid JSON).
	control := mustCreate(t, store, beads.Bead{
		Title: "review",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review",
			"gc.step_id":          "review",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": `{not valid json`,
			"gc.control_epoch":    "1",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "review attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.step_ref":       "mol-test.review.attempt.1",
			"gc.attempt":        "1",
			"gc.outcome":        "fail",
			"gc.failure_class":  "transient",
			"gc.failure_reason": "rate_limited",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	_, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err == nil {
		t.Fatal("expected error from bad source_step_spec")
	}

	// The control should have been closed with controller_error disposition.
	after := mustGet(t, store, control.ID)
	if after.Status != "closed" {
		t.Fatalf("control status = %q, want closed (controller_error)", after.Status)
	}
	if after.Metadata["gc.final_disposition"] != "controller_error" {
		t.Fatalf("disposition = %q, want controller_error", after.Metadata["gc.final_disposition"])
	}
	if after.Metadata["gc.controller_error"] == "" {
		t.Fatal("gc.controller_error should be set")
	}
}

// ---------------------------------------------------------------------------
// findLatestAttempt tests
// ---------------------------------------------------------------------------

func TestFindLatestAttemptDirectRef(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})

	control := mustCreate(t, store, beads.Bead{
		Title: "review retry",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review",
			"gc.step_id":      "review",
		},
	})

	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review.attempt.1",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, attempt1.ID)

	found, err := findLatestAttempt(store, mustGet(t, store, control.ID))
	if err != nil {
		t.Fatalf("findLatestAttempt: %v", err)
	}
	if found.ID != attempt1.ID {
		t.Fatalf("findLatestAttempt returned %q, want %q", found.ID, attempt1.ID)
	}
}

func TestFindLatestAttemptMultipleAttempts(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})

	control := mustCreate(t, store, beads.Bead{
		Title: "review retry",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review",
			"gc.step_id":      "review",
		},
	})

	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review.attempt.1",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, attempt1.ID)

	attempt2 := mustCreate(t, store, beads.Bead{
		Title: "attempt 2",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review.attempt.2",
			"gc.attempt":      "2",
		},
	})
	mustClose(t, store, attempt2.ID)

	found, err := findLatestAttempt(store, mustGet(t, store, control.ID))
	if err != nil {
		t.Fatalf("findLatestAttempt: %v", err)
	}
	if found.ID != attempt2.ID {
		t.Fatalf("findLatestAttempt returned %q, want %q (latest attempt)", found.ID, attempt2.ID)
	}
}

func TestFindLatestAttemptNestedRetryInsideRalph(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})

	// Retry control inside a ralph iteration -- step_ref is fully namespaced.
	control := mustCreate(t, store, beads.Bead{
		Title: "review-own-code retry",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-demo.self-review.iteration.1.review-own-code",
			"gc.step_id":      "self-review",
		},
	})

	// Attempt bead -- step_ref is SHORT (bare child ID, not fully namespaced).
	attempt := mustCreate(t, store, beads.Bead{
		Title: "review-own-code attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "review-own-code.attempt.1",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, attempt.ID)

	// Scope-check with gc.attempt set -- should be skipped by findLatestAttempt.
	scopeCheck := mustCreate(t, store, beads.Bead{
		Title: "scope-check for attempt",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, scopeCheck.ID)
	mustDep(t, store, control.ID, scopeCheck.ID, "blocks")

	found, err := findLatestAttempt(store, mustGet(t, store, control.ID))
	if err != nil {
		t.Fatalf("findLatestAttempt: %v", err)
	}
	if found.ID != attempt.ID {
		t.Fatalf("findLatestAttempt returned %q, want %q (attempt bead)", found.ID, attempt.ID)
	}
}

func TestFindLatestAttemptRalphIteration(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})

	control := mustCreate(t, store, beads.Bead{
		Title: "self-review ralph",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-demo.self-review",
			"gc.step_id":      "self-review",
		},
	})

	iteration := mustCreate(t, store, beads.Bead{
		Title: "self-review iteration 1",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-demo.self-review.iteration.1",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, iteration.ID)

	found, err := findLatestAttempt(store, mustGet(t, store, control.ID))
	if err != nil {
		t.Fatalf("findLatestAttempt: %v", err)
	}
	if found.ID != iteration.ID {
		t.Fatalf("findLatestAttempt returned %q, want %q (scope iteration)", found.ID, iteration.ID)
	}
}

func TestFindLatestAttemptScopeCheckNotMatched(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})

	control := mustCreate(t, store, beads.Bead{
		Title: "review retry",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review",
			"gc.step_id":      "review",
		},
	})

	// A scope-check bead with gc.attempt set. Even though it has gc.attempt,
	// its gc.kind=scope-check should cause it to be skipped.
	mustCreate(t, store, beads.Bead{
		Title: "scope-check bead",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review.attempt.1",
			"gc.attempt":      "1",
		},
	})

	// The actual attempt bead.
	realAttempt := mustCreate(t, store, beads.Bead{
		Title: "real attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-feature.review.attempt.1",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, realAttempt.ID)

	found, err := findLatestAttempt(store, mustGet(t, store, control.ID))
	if err != nil {
		t.Fatalf("findLatestAttempt: %v", err)
	}
	if found.ID != realAttempt.ID {
		t.Fatalf("findLatestAttempt returned %q, want %q (scope-check should be skipped)", found.ID, realAttempt.ID)
	}
}

// ---------------------------------------------------------------------------
// buildAttemptRecipe tests
// ---------------------------------------------------------------------------

func TestBuildAttemptRecipeSimpleRetry(t *testing.T) {
	t.Parallel()

	step := &formula.Step{
		ID:     "review",
		Title:  "Review code",
		Type:   "task",
		Labels: []string{"pool:polecat"},
		Retry:  &formula.RetrySpec{MaxAttempts: 3},
	}

	control := beads.Bead{
		ID: "gc-1",
		Metadata: map[string]string{
			"gc.step_id":  "review",
			"gc.step_ref": "mol-test.review",
		},
	}

	recipe := buildAttemptRecipe(step, control, 2)

	// Recipe name uses fully namespaced step_ref.
	if recipe.Name != "mol-test.review.attempt.2" {
		t.Errorf("recipe name = %q, want mol-test.review.attempt.2", recipe.Name)
	}
	if len(recipe.Steps) != 1 {
		t.Fatalf("steps = %d, want 1 (simple retry has one step)", len(recipe.Steps))
	}

	rootStep := recipe.Steps[0]
	// Step ID should use fully namespaced ref.
	if rootStep.ID != "mol-test.review.attempt.2" {
		t.Errorf("step ID = %q, want mol-test.review.attempt.2", rootStep.ID)
	}
	if rootStep.Metadata["gc.attempt"] != "2" {
		t.Errorf("gc.attempt = %q, want 2", rootStep.Metadata["gc.attempt"])
	}
	if rootStep.Metadata["gc.step_ref"] != "mol-test.review.attempt.2" {
		t.Errorf("gc.step_ref = %q, want mol-test.review.attempt.2", rootStep.Metadata["gc.step_ref"])
	}
	if rootStep.Metadata["gc.step_id"] != "review" {
		t.Errorf("gc.step_id = %q, want review", rootStep.Metadata["gc.step_id"])
	}
	if !rootStep.IsRoot {
		t.Error("root step should have IsRoot=true")
	}
}

func TestBuildAttemptRecipeRalphWithChildren(t *testing.T) {
	t.Parallel()

	step := &formula.Step{
		ID:    "converge",
		Title: "Converge",
		Type:  "task",
		Ralph: &formula.RalphSpec{MaxAttempts: 5},
		Children: []*formula.Step{
			{ID: "apply", Title: "Apply", Type: "task"},
			{ID: "verify", Title: "Verify", Type: "task", Needs: []string{"apply"}},
		},
	}

	control := beads.Bead{
		ID: "gc-1",
		Metadata: map[string]string{
			"gc.step_id":  "converge",
			"gc.step_ref": "mol-test.converge",
		},
	}

	recipe := buildAttemptRecipe(step, control, 3)

	// Ralph uses .iteration.N naming.
	if recipe.Name != "mol-test.converge.iteration.3" {
		t.Errorf("recipe name = %q, want mol-test.converge.iteration.3", recipe.Name)
	}
	if len(recipe.Steps) != 3 {
		t.Fatalf("steps = %d, want 3 (root + 2 children)", len(recipe.Steps))
	}

	// Root scope step.
	if recipe.Steps[0].ID != "mol-test.converge.iteration.3" {
		t.Errorf("root ID = %q, want mol-test.converge.iteration.3", recipe.Steps[0].ID)
	}
	if recipe.Steps[0].Metadata["gc.kind"] != "scope" {
		t.Errorf("root gc.kind = %q, want scope", recipe.Steps[0].Metadata["gc.kind"])
	}
	if recipe.Steps[0].Metadata["gc.step_ref"] != "mol-test.converge.iteration.3" {
		t.Errorf("root gc.step_ref = %q, want mol-test.converge.iteration.3", recipe.Steps[0].Metadata["gc.step_ref"])
	}

	// Children with fully namespaced IDs.
	if recipe.Steps[1].ID != "mol-test.converge.iteration.3.apply" {
		t.Errorf("child 1 ID = %q, want mol-test.converge.iteration.3.apply", recipe.Steps[1].ID)
	}
	if recipe.Steps[1].Metadata["gc.step_ref"] != "mol-test.converge.iteration.3.apply" {
		t.Errorf("child 1 gc.step_ref = %q, want mol-test.converge.iteration.3.apply", recipe.Steps[1].Metadata["gc.step_ref"])
	}
	if recipe.Steps[1].Metadata["gc.attempt"] != "3" {
		t.Errorf("child 1 gc.attempt = %q, want 3", recipe.Steps[1].Metadata["gc.attempt"])
	}

	if recipe.Steps[2].ID != "mol-test.converge.iteration.3.verify" {
		t.Errorf("child 2 ID = %q, want mol-test.converge.iteration.3.verify", recipe.Steps[2].ID)
	}

	// Verify should block on apply (namespaced).
	foundBlocksDep := false
	for _, dep := range recipe.Deps {
		if dep.StepID == "mol-test.converge.iteration.3.verify" &&
			dep.DependsOnID == "mol-test.converge.iteration.3.apply" &&
			dep.Type == "blocks" {
			foundBlocksDep = true
		}
	}
	if !foundBlocksDep {
		t.Errorf("missing dep: verify blocks on apply; deps = %+v", recipe.Deps)
	}

	// Children should NOT have parent-child deps to the scope root —
	// parent-child creates a deadlock (scope waits for children, children
	// wait for scope). Containment is expressed via gc.scope_ref metadata.
	for _, dep := range recipe.Deps {
		if dep.Type == "parent-child" {
			t.Errorf("unexpected parent-child dep: %s -> %s (causes deadlock)", dep.StepID, dep.DependsOnID)
		}
	}
}

func TestBuildAttemptRecipeUsesFullyNamespacedStepRef(t *testing.T) {
	t.Parallel()

	// When gc.step_ref is set on the control, the recipe should use it
	// as the prefix, not the bare gc.step_id.
	step := &formula.Step{
		ID:    "lint",
		Title: "Lint",
		Type:  "task",
		Retry: &formula.RetrySpec{MaxAttempts: 2},
	}

	control := beads.Bead{
		ID: "gc-99",
		Metadata: map[string]string{
			"gc.step_id":  "lint",
			"gc.step_ref": "mol-big-workflow.phase-1.lint",
		},
	}

	recipe := buildAttemptRecipe(step, control, 1)

	if recipe.Name != "mol-big-workflow.phase-1.lint.attempt.1" {
		t.Errorf("recipe name = %q, want mol-big-workflow.phase-1.lint.attempt.1", recipe.Name)
	}
	if recipe.Steps[0].ID != "mol-big-workflow.phase-1.lint.attempt.1" {
		t.Errorf("step ID = %q, want mol-big-workflow.phase-1.lint.attempt.1", recipe.Steps[0].ID)
	}
}

// ---------------------------------------------------------------------------
// appendAttemptLog tests
// ---------------------------------------------------------------------------

func TestAttemptLogMultipleEntries(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	bead, _ := store.Create(beads.Bead{Title: "test", Metadata: map[string]string{}})

	if err := appendAttemptLog(store, bead.ID, 1, "transient", "rate_limited"); err != nil {
		t.Fatalf("appendAttemptLog 1: %v", err)
	}
	if err := appendAttemptLog(store, bead.ID, 2, "pass", ""); err != nil {
		t.Fatalf("appendAttemptLog 2: %v", err)
	}

	after, _ := store.Get(bead.ID)
	var log []map[string]string
	if err := json.Unmarshal([]byte(after.Metadata["gc.attempt_log"]), &log); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(log) != 2 {
		t.Fatalf("log entries = %d, want 2", len(log))
	}
	if log[0]["attempt"] != "1" || log[0]["outcome"] != "transient" || log[0]["action"] != "retry" {
		t.Errorf("log[0] = %v, want attempt:1 outcome:transient action:retry", log[0])
	}
	if log[1]["attempt"] != "2" || log[1]["outcome"] != "pass" || log[1]["action"] != "close" {
		t.Errorf("log[1] = %v, want attempt:2 outcome:pass action:close", log[1])
	}
}

func TestAttemptLogJSONRoundTrips(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	bead, _ := store.Create(beads.Bead{Title: "test", Metadata: map[string]string{}})

	if err := appendAttemptLog(store, bead.ID, 1, "hard", "auth_error"); err != nil {
		t.Fatalf("appendAttemptLog: %v", err)
	}

	after, _ := store.Get(bead.ID)
	raw := after.Metadata["gc.attempt_log"]

	// Verify it's valid JSON.
	var parsed []map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v (raw = %q)", err, raw)
	}

	// Re-marshal and unmarshal to verify round-trip.
	reMarshaled, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var roundTripped []map[string]string
	if err := json.Unmarshal(reMarshaled, &roundTripped); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}

	if len(roundTripped) != 1 {
		t.Fatalf("round-trip entries = %d, want 1", len(roundTripped))
	}
	if roundTripped[0]["attempt"] != "1" || roundTripped[0]["outcome"] != "hard" || roundTripped[0]["action"] != "hard-fail" {
		t.Errorf("round-trip log[0] = %v, want attempt:1 outcome:hard action:hard-fail", roundTripped[0])
	}
	if roundTripped[0]["reason"] != "auth_error" {
		t.Errorf("round-trip log[0].reason = %q, want auth_error", roundTripped[0]["reason"])
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func mustCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("create %q: %v", b.Title, err)
	}
	for k, v := range b.Metadata {
		if created.Metadata[k] != v {
			if err := store.SetMetadata(created.ID, k, v); err != nil {
				t.Fatalf("set metadata %s=%s: %v", k, v, err)
			}
		}
	}
	created, _ = store.Get(created.ID)
	return created
}

func mustClose(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
}

func mustDep(t *testing.T, store beads.Store, from, to, depType string) { //nolint:unparam // depType is "blocks" in current tests; kept parameterized for future dep types
	t.Helper()
	if err := store.DepAdd(from, to, depType); err != nil {
		t.Fatalf("dep %s -> %s: %v", from, to, err)
	}
}

// ---------------------------------------------------------------------------
// Regression: scope bead must block on children (not parent-child deadlock)
// ---------------------------------------------------------------------------

func TestBuildAttemptRecipeScopeBlocksOnAllChildren(t *testing.T) {
	t.Parallel()

	// The scope bead must have blocks deps on ALL top-level children.
	// Without this, the scope stays open forever (nothing closes it).
	step := &formula.Step{
		ID:    "review-loop",
		Title: "Review loop",
		Ralph: &formula.RalphSpec{MaxAttempts: 5},
		Children: []*formula.Step{
			{ID: "review-claude", Title: "Claude"},
			{ID: "review-codex", Title: "Codex"},
			{ID: "synthesize", Title: "Synthesize", Needs: []string{"review-claude", "review-codex"}},
			{ID: "apply-fixes", Title: "Apply fixes", Needs: []string{"synthesize"}},
		},
	}

	control := beads.Bead{
		ID: "ctrl-1",
		Metadata: map[string]string{
			"gc.step_id":  "review-loop",
			"gc.step_ref": "mol.review-loop",
		},
	}

	recipe := buildAttemptRecipe(step, control, 1)
	scopeID := "mol.review-loop.iteration.1"

	// Scope must block on each child.
	expectedBlockers := []string{
		scopeID + ".review-claude",
		scopeID + ".review-codex",
		scopeID + ".synthesize",
		scopeID + ".apply-fixes",
	}

	scopeDeps := map[string]bool{}
	for _, dep := range recipe.Deps {
		if dep.StepID == scopeID && dep.Type == "blocks" {
			scopeDeps[dep.DependsOnID] = true
		}
	}

	for _, expected := range expectedBlockers {
		if !scopeDeps[expected] {
			t.Errorf("scope %q missing blocks dep on %q; scope deps = %v", scopeID, expected, scopeDeps)
		}
	}
}

func TestBuildAttemptRecipeNoParentChildDeps(t *testing.T) {
	t.Parallel()

	// Regression: parent-child deps from children→scope create a deadlock
	// because scope waits for children (blocks) and children wait for
	// scope (parent-child). Only blocks deps should exist.
	step := &formula.Step{
		ID:    "loop",
		Title: "Loop",
		Ralph: &formula.RalphSpec{MaxAttempts: 3},
		Children: []*formula.Step{
			{ID: "work", Title: "Work"},
			{ID: "check", Title: "Check", Needs: []string{"work"}},
		},
	}

	control := beads.Bead{
		ID:       "ctrl-2",
		Metadata: map[string]string{"gc.step_id": "loop", "gc.step_ref": "mol.loop"},
	}

	recipe := buildAttemptRecipe(step, control, 1)

	for _, dep := range recipe.Deps {
		if dep.Type == "parent-child" {
			t.Errorf("parent-child dep found: %s → %s (causes deadlock)", dep.StepID, dep.DependsOnID)
		}
	}
}

func TestBuildAttemptRecipeComposeExpandFanout(t *testing.T) {
	t.Parallel()

	// Real-world case: compose.expand produces multi-segment child IDs
	// like "review-pipeline.review-claude". These children also have retry.
	// Verify: scope blocks on children, no parent-child, inter-child deps correct.
	step := &formula.Step{
		ID:    "review-loop",
		Title: "Review loop",
		Ralph: &formula.RalphSpec{MaxAttempts: 999},
		Children: []*formula.Step{
			{
				ID:    "review-pipeline.review-claude",
				Title: "Claude review",
				Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
			},
			{
				ID:    "review-pipeline.review-codex",
				Title: "Codex review",
				Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
			},
			{
				ID:    "review-pipeline.synthesize",
				Title: "Synthesize",
				Needs: []string{"review-pipeline.review-claude", "review-pipeline.review-codex"},
				Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
			},
			{
				ID:    "apply-fixes",
				Title: "Apply fixes",
				Needs: []string{"review-pipeline.synthesize"},
				Retry: &formula.RetrySpec{MaxAttempts: 3, OnExhausted: "hard_fail"},
			},
		},
	}

	control := beads.Bead{
		ID: "ctrl-3",
		Metadata: map[string]string{
			"gc.step_id":  "review-loop",
			"gc.step_ref": "mol-adopt-pr-v2.review-loop",
		},
	}

	recipe := buildAttemptRecipe(step, control, 1)
	scopeID := "mol-adopt-pr-v2.review-loop.iteration.1"

	// No parent-child deps.
	for _, dep := range recipe.Deps {
		if dep.Type == "parent-child" {
			t.Errorf("parent-child dep: %s → %s", dep.StepID, dep.DependsOnID)
		}
	}

	// Scope blocks on all 4 children.
	scopeBlockers := map[string]bool{}
	for _, dep := range recipe.Deps {
		if dep.StepID == scopeID && dep.Type == "blocks" {
			scopeBlockers[dep.DependsOnID] = true
		}
	}
	for _, childID := range []string{
		scopeID + ".review-pipeline.review-claude",
		scopeID + ".review-pipeline.review-codex",
		scopeID + ".review-pipeline.synthesize",
		scopeID + ".apply-fixes",
	} {
		if !scopeBlockers[childID] {
			t.Errorf("scope missing blocks dep on %q", childID)
		}
	}

	// Synthesize blocks on both reviewers.
	synthID := scopeID + ".review-pipeline.synthesize"
	synthBlockers := map[string]bool{}
	for _, dep := range recipe.Deps {
		if dep.StepID == synthID && dep.Type == "blocks" {
			synthBlockers[dep.DependsOnID] = true
		}
	}
	if !synthBlockers[scopeID+".review-pipeline.review-claude"] {
		t.Errorf("synthesize missing dep on review-claude")
	}
	if !synthBlockers[scopeID+".review-pipeline.review-codex"] {
		t.Errorf("synthesize missing dep on review-codex")
	}

	// Apply-fixes blocks on synthesize.
	applyID := scopeID + ".apply-fixes"
	foundApplyDep := false
	for _, dep := range recipe.Deps {
		if dep.StepID == applyID && dep.DependsOnID == synthID && dep.Type == "blocks" {
			foundApplyDep = true
		}
	}
	if !foundApplyDep {
		t.Errorf("apply-fixes missing blocks dep on synthesize")
	}

	// Children with retry should have gc.kind=retry in metadata.
	foundRetryStep := false
	foundSpecBead := false
	for _, s := range recipe.Steps {
		if s.ID == scopeID+".review-pipeline.review-claude" {
			foundRetryStep = true
			if s.Metadata["gc.kind"] != "retry" {
				t.Errorf("review-claude gc.kind = %q, want retry", s.Metadata["gc.kind"])
			}
		}
		if s.ID == scopeID+".review-pipeline.review-claude.spec" {
			foundSpecBead = true
			if s.Metadata["gc.kind"] != "spec" {
				t.Errorf("review-claude spec gc.kind = %q, want spec", s.Metadata["gc.kind"])
			}
		}
	}
	if !foundRetryStep {
		t.Error("review-claude retry step not found")
	}
	if !foundSpecBead {
		t.Error("review-claude missing spec bead for frozen step spec")
	}
}

func TestBuildAttemptRecipeScopeMetadataAndStepRef(t *testing.T) {
	t.Parallel()

	// Verify scope bead has correct metadata for ralph iterations.
	step := &formula.Step{
		ID:    "loop",
		Title: "Loop",
		Ralph: &formula.RalphSpec{MaxAttempts: 3},
		Children: []*formula.Step{
			{ID: "work", Title: "Work"},
		},
	}

	control := beads.Bead{
		ID:       "ctrl-4",
		Metadata: map[string]string{"gc.step_id": "loop", "gc.step_ref": "mol.loop"},
	}

	recipe := buildAttemptRecipe(step, control, 2)
	scope := recipe.Steps[0]

	if scope.Metadata["gc.kind"] != "scope" {
		t.Errorf("scope gc.kind = %q, want scope", scope.Metadata["gc.kind"])
	}
	if scope.Metadata["gc.scope_role"] != "body" {
		t.Errorf("scope gc.scope_role = %q, want body", scope.Metadata["gc.scope_role"])
	}
	if scope.Metadata["gc.attempt"] != "2" {
		t.Errorf("scope gc.attempt = %q, want 2", scope.Metadata["gc.attempt"])
	}
	if scope.Metadata["gc.step_ref"] != "mol.loop.iteration.2" {
		t.Errorf("scope gc.step_ref = %q, want mol.loop.iteration.2", scope.Metadata["gc.step_ref"])
	}
}

func mustGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

// Unused import guard.
var _ = strconv.Itoa
