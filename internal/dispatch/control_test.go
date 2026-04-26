package dispatch

import (
	"encoding/json"
	"errors"
	"strconv"
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

func TestProcessRetryControlPassClosesWithSingleFinalMetadataUpdate(t *testing.T) {
	t.Parallel()
	base := beads.NewMemStore()

	root := mustCreate(t, base, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, base, beads.Bead{
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
	attempt1 := mustCreate(t, base, beads.Bead{
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
	mustClose(t, base, attempt1.ID)
	mustDep(t, base, control.ID, attempt1.ID, "blocks")

	store := &controlCloseTrackingStore{Store: base, targetID: control.ID}
	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}
	if store.setMetadataCalls != 0 || store.setMetadataBatchCalls != 0 {
		t.Fatalf("metadata calls before close = SetMetadata:%d SetMetadataBatch:%d, want none", store.setMetadataCalls, store.setMetadataBatchCalls)
	}
	if store.closeUpdateCalls != 1 {
		t.Fatalf("close update calls = %d, want 1", store.closeUpdateCalls)
	}
	for key, want := range map[string]string{
		"gc.outcome":     "pass",
		"gc.output_json": `{"ok":true}`,
		"review.verdict": "approved",
	} {
		if got := store.closeUpdateMetadata[key]; got != want {
			t.Fatalf("close metadata %s = %q, want %q", key, got, want)
		}
	}
	if store.closeUpdateMetadata["gc.attempt_log"] == "" {
		t.Fatal("close metadata missing gc.attempt_log")
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

func TestProcessRetryControlClosesEnclosingScopeOnFailure(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	scopeBody := mustCreate(t, store, beads.Bead{
		Title: "review iteration",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.2",
			"gc.scope_role":   "body",
		},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "apply fixes",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review-loop.iteration.2.apply-fixes",
			"gc.step_id":          "apply-fixes",
			"gc.scope_ref":        "mol-test.review-loop.iteration.2",
			"gc.scope_role":       "member",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": `{"id":"apply-fixes","title":"Apply fixes","type":"task","retry":{"max_attempts":3}}`,
			"gc.control_epoch":    "1",
		},
	})
	sibling := mustCreate(t, store, beads.Bead{
		Title: "cleanup note",
		Metadata: map[string]string{
			"gc.kind":         "task",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.2.cleanup-note",
			"gc.scope_ref":    "mol-test.review-loop.iteration.2",
			"gc.scope_role":   "member",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "apply fixes attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.step_ref":       "mol-test.review-loop.iteration.2.apply-fixes.attempt.1",
			"gc.attempt":        "1",
			"gc.outcome":        "fail",
			"gc.failure_class":  "hard",
			"gc.failure_reason": "missing_review_artifact",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")
	mustDep(t, store, scopeBody.ID, control.ID, "blocks")
	mustDep(t, store, scopeBody.ID, sibling.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if !result.Processed || result.Action != "hard-fail" {
		t.Fatalf("result = %+v, want processed hard-fail", result)
	}
	if result.Skipped != 1 {
		t.Fatalf("result.Skipped = %d, want 1", result.Skipped)
	}

	controlAfter := mustGet(t, store, control.ID)
	if controlAfter.Status != "closed" || controlAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("control = status %q outcome %q, want closed/fail", controlAfter.Status, controlAfter.Metadata["gc.outcome"])
	}

	scopeAfter := mustGet(t, store, scopeBody.ID)
	if scopeAfter.Status != "closed" || scopeAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("scope body = status %q outcome %q, want closed/fail", scopeAfter.Status, scopeAfter.Metadata["gc.outcome"])
	}

	siblingAfter := mustGet(t, store, sibling.ID)
	if siblingAfter.Status != "closed" || siblingAfter.Metadata["gc.outcome"] != "skipped" {
		t.Fatalf("sibling = status %q outcome %q, want closed/skipped", siblingAfter.Status, siblingAfter.Metadata["gc.outcome"])
	}
}

func TestProcessRetryControlClosesEnclosingScopeOnPassAndPropagatesMetadata(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	scopeBody := mustCreate(t, store, beads.Bead{
		Title: "review iteration",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.2",
			"gc.scope_role":   "body",
		},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "apply fixes",
		Metadata: map[string]string{
			"gc.kind":             "retry",
			"gc.root_bead_id":     root.ID,
			"gc.step_ref":         "mol-test.review-loop.iteration.2.apply-fixes",
			"gc.step_id":          "apply-fixes",
			"gc.scope_ref":        "mol-test.review-loop.iteration.2",
			"gc.scope_role":       "member",
			"gc.max_attempts":     "3",
			"gc.on_exhausted":     "hard_fail",
			"gc.source_step_spec": `{"id":"apply-fixes","title":"Apply fixes","type":"task","retry":{"max_attempts":3}}`,
			"gc.control_epoch":    "1",
		},
	})
	attempt1 := mustCreate(t, store, beads.Bead{
		Title: "apply fixes attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.2.apply-fixes.attempt.1",
			"gc.attempt":      "1",
			"gc.outcome":      "pass",
			"gc.output_json":  `{"verdict":"done"}`,
			"review.verdict":  "done",
			"review.summary":  "artifact restored",
		},
	})
	mustClose(t, store, attempt1.ID)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")
	mustDep(t, store, scopeBody.ID, control.ID, "blocks")

	result, err := processRetryControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRetryControl: %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	controlAfter := mustGet(t, store, control.ID)
	if controlAfter.Status != "closed" || controlAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("control = status %q outcome %q, want closed/pass", controlAfter.Status, controlAfter.Metadata["gc.outcome"])
	}

	scopeAfter := mustGet(t, store, scopeBody.ID)
	if scopeAfter.Status != "closed" || scopeAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("scope body = status %q outcome %q, want closed/pass", scopeAfter.Status, scopeAfter.Metadata["gc.outcome"])
	}
	if scopeAfter.Metadata["gc.output_json"] != `{"verdict":"done"}` {
		t.Fatalf("scope body gc.output_json = %q, want propagated output", scopeAfter.Metadata["gc.output_json"])
	}
	if scopeAfter.Metadata["review.verdict"] != "done" {
		t.Fatalf("scope body review.verdict = %q, want done", scopeAfter.Metadata["review.verdict"])
	}
	if scopeAfter.Metadata["review.summary"] != "artifact restored" {
		t.Fatalf("scope body review.summary = %q, want artifact restored", scopeAfter.Metadata["review.summary"])
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
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("error = %v, want %v", err, ErrControlPending)
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

func TestFindLatestAttemptFallsBackToDirectDependencyWhenRootIsScoped(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	workflow := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	scope := mustCreate(t, store, beads.Bead{
		Title: "review-loop iteration 2",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol-adopt-pr-v2.review-loop.iteration.2",
			"gc.attempt":      "2",
		},
	})

	control := mustCreate(t, store, beads.Bead{
		Title: "review-codex retry",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": scope.ID,
			"gc.step_ref":     "mol-adopt-pr-v2.review-loop.iteration.2.review-pipeline.review-codex",
			"gc.step_id":      "review-pipeline.review-codex",
		},
	})

	// Live integration failure shape: the retry wrapper is rooted to the
	// scoped iteration bead, but the actual attempt bead still carries the
	// workflow root and is only discoverable through the direct block edge.
	attempt := mustCreate(t, store, beads.Bead{
		Title: "review-codex attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id": workflow.ID,
			"gc.step_ref":     "mol-adopt-pr-v2.review-loop.iteration.2.review-pipeline.review-codex.attempt.1",
			"gc.attempt":      "2",
		},
	})
	mustClose(t, store, attempt.ID)
	mustDep(t, store, control.ID, attempt.ID, "blocks")

	found, err := findLatestAttempt(store, mustGet(t, store, control.ID))
	if err != nil {
		t.Fatalf("findLatestAttempt: %v", err)
	}
	if found.ID != attempt.ID {
		t.Fatalf("findLatestAttempt returned %q, want %q (direct dependency fallback)", found.ID, attempt.ID)
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

func TestProcessRalphControlClosesEnclosingScopeOnIterationFailure(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	scopeBody := mustCreate(t, store, beads.Bead{
		Title: "outer scope",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.outer-scope",
			"gc.scope_role":   "body",
		},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop",
			"gc.step_id":      "review-loop",
			"gc.scope_ref":    "mol-test.outer-scope",
			"gc.scope_role":   "member",
			"gc.max_attempts": "1",
		},
	})
	iteration := mustCreate(t, store, beads.Bead{
		Title: "review loop iteration 1",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.1",
			"gc.scope_role":   "body",
			"gc.attempt":      "1",
			"gc.outcome":      "fail",
		},
	})
	mustClose(t, store, iteration.ID)
	mustDep(t, store, control.ID, iteration.ID, "blocks")
	mustDep(t, store, scopeBody.ID, control.ID, "blocks")

	result, err := processRalphControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("processRalphControl: %v", err)
	}
	if !result.Processed || result.Action != "fail" {
		t.Fatalf("result = %+v, want processed fail", result)
	}

	controlAfter := mustGet(t, store, control.ID)
	if controlAfter.Status != "closed" || controlAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("control = status %q outcome %q, want closed/fail", controlAfter.Status, controlAfter.Metadata["gc.outcome"])
	}

	scopeAfter := mustGet(t, store, scopeBody.ID)
	if scopeAfter.Status != "closed" || scopeAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("scope body = status %q outcome %q, want closed/fail", scopeAfter.Status, scopeAfter.Metadata["gc.outcome"])
	}
}

func TestProcessRalphControlReturnsPendingForOpenIteration(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop",
			"gc.step_id":      "review-loop",
			"gc.max_attempts": "2",
		},
	})
	iteration := mustCreate(t, store, beads.Bead{
		Title: "review loop iteration 1",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.1",
			"gc.scope_role":   "body",
			"gc.attempt":      "1",
		},
	})
	mustDep(t, store, control.ID, iteration.ID, "blocks")

	_, err := processRalphControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("error = %v, want %v", err, ErrControlPending)
	}
}

// TestReconcileClosedScopeMemberRalphPass covers the pass-side symmetry of
// TestProcessRalphControlClosesEnclosingScopeOnIterationFailure: when a scoped
// ralph control closes with gc.outcome=pass, reconcileClosedScopeMember must
// auto-close the enclosing scope body with outcome=pass. Exercises the wiring
// on control.go:176-183 without running the full check pipeline (which would
// require an executable check script).
func TestReconcileClosedScopeMemberRalphPass(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	scopeBody := mustCreate(t, store, beads.Bead{
		Title: "outer scope",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.outer-scope",
			"gc.scope_role":   "body",
		},
	})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop",
			"gc.step_id":      "review-loop",
			"gc.scope_ref":    "mol-test.outer-scope",
			"gc.scope_role":   "member",
			"gc.max_attempts": "3",
		},
	})
	mustDep(t, store, scopeBody.ID, control.ID, "blocks")

	// Simulate the terminal-pass close that processRalphControl performs
	// at control.go:176 after a check returns GatePass.
	if err := setOutcomeAndClose(store, control.ID, "pass"); err != nil {
		t.Fatalf("setOutcomeAndClose: %v", err)
	}

	if _, err := reconcileClosedScopeMember(store, control.ID); err != nil {
		t.Fatalf("reconcileClosedScopeMember: %v", err)
	}

	scopeAfter := mustGet(t, store, scopeBody.ID)
	if scopeAfter.Status != "closed" || scopeAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("scope body = status %q outcome %q, want closed/pass", scopeAfter.Status, scopeAfter.Metadata["gc.outcome"])
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
	if len(recipe.Steps) != 5 {
		t.Fatalf("steps = %d, want 5 (root + 2 children + 2 scope-checks)", len(recipe.Steps))
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

	applyStep := recipe.StepByID("mol-test.converge.iteration.3.apply")
	if applyStep == nil {
		t.Fatal("missing apply step")
	}
	if applyStep.Metadata["gc.step_ref"] != "mol-test.converge.iteration.3.apply" {
		t.Errorf("apply gc.step_ref = %q, want mol-test.converge.iteration.3.apply", applyStep.Metadata["gc.step_ref"])
	}
	if applyStep.Metadata["gc.attempt"] != "3" {
		t.Errorf("apply gc.attempt = %q, want 3", applyStep.Metadata["gc.attempt"])
	}

	verifyStep := recipe.StepByID("mol-test.converge.iteration.3.verify")
	if verifyStep == nil {
		t.Fatal("missing verify step")
	}
	applyScopeCheck := recipe.StepByID("mol-test.converge.iteration.3.apply-scope-check")
	if applyScopeCheck == nil {
		t.Fatal("missing apply scope-check")
	}
	if applyScopeCheck.Metadata["gc.kind"] != "scope-check" {
		t.Errorf("apply scope-check gc.kind = %q, want scope-check", applyScopeCheck.Metadata["gc.kind"])
	}
	if applyScopeCheck.Metadata["gc.control_for"] != "mol-test.converge.iteration.3.apply" {
		t.Errorf("apply scope-check gc.control_for = %q, want mol-test.converge.iteration.3.apply", applyScopeCheck.Metadata["gc.control_for"])
	}
	verifyScopeCheck := recipe.StepByID("mol-test.converge.iteration.3.verify-scope-check")
	if verifyScopeCheck == nil {
		t.Fatal("missing verify scope-check")
	}

	// Verify should block on apply (namespaced).
	foundBlocksDep := false
	foundScopeControlDep := false
	foundScopeBodyDep := false
	for _, dep := range recipe.Deps {
		if dep.StepID == "mol-test.converge.iteration.3.verify" &&
			dep.DependsOnID == "mol-test.converge.iteration.3.apply-scope-check" &&
			dep.Type == "blocks" {
			foundBlocksDep = true
		}
		if dep.StepID == "mol-test.converge.iteration.3.apply-scope-check" &&
			dep.DependsOnID == "mol-test.converge.iteration.3.apply" &&
			dep.Type == "blocks" {
			foundScopeControlDep = true
		}
		if dep.StepID == "mol-test.converge.iteration.3" &&
			dep.DependsOnID == "mol-test.converge.iteration.3.verify-scope-check" &&
			dep.Type == "blocks" {
			foundScopeBodyDep = true
		}
	}
	if !foundBlocksDep {
		t.Errorf("missing dep: verify blocks on apply scope-check; deps = %+v", recipe.Deps)
	}
	if !foundScopeControlDep {
		t.Errorf("missing dep: apply scope-check blocks on apply; deps = %+v", recipe.Deps)
	}
	if !foundScopeBodyDep {
		t.Errorf("missing dep: scope body blocks on verify scope-check; deps = %+v", recipe.Deps)
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

type controlCloseTrackingStore struct {
	beads.Store
	targetID              string
	setMetadataCalls      int
	setMetadataBatchCalls int
	closeUpdateCalls      int
	closeUpdateMetadata   map[string]string
}

func (s *controlCloseTrackingStore) SetMetadata(id, key, value string) error {
	if id == s.targetID {
		s.setMetadataCalls++
	}
	return s.Store.SetMetadata(id, key, value)
}

func (s *controlCloseTrackingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.targetID {
		s.setMetadataBatchCalls++
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *controlCloseTrackingStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.targetID && opts.Status != nil && *opts.Status == "closed" {
		s.closeUpdateCalls++
		s.closeUpdateMetadata = make(map[string]string, len(opts.Metadata))
		for key, value := range opts.Metadata {
			s.closeUpdateMetadata[key] = value
		}
	}
	return s.Store.Update(id, opts)
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
		scopeID + ".review-claude-scope-check",
		scopeID + ".review-codex-scope-check",
		scopeID + ".synthesize-scope-check",
		scopeID + ".apply-fixes-scope-check",
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

	// Scope blocks on all 4 child scope-check controls.
	scopeBlockers := map[string]bool{}
	for _, dep := range recipe.Deps {
		if dep.StepID == scopeID && dep.Type == "blocks" {
			scopeBlockers[dep.DependsOnID] = true
		}
	}
	for _, childID := range []string{
		scopeID + ".review-pipeline.review-claude-scope-check",
		scopeID + ".review-pipeline.review-codex-scope-check",
		scopeID + ".review-pipeline.synthesize-scope-check",
		scopeID + ".apply-fixes-scope-check",
	} {
		if !scopeBlockers[childID] {
			t.Errorf("scope missing blocks dep on %q", childID)
		}
	}

	// Synthesize blocks on both reviewer scope-check controls.
	synthID := scopeID + ".review-pipeline.synthesize"
	synthBlockers := map[string]bool{}
	for _, dep := range recipe.Deps {
		if dep.StepID == synthID && dep.Type == "blocks" {
			synthBlockers[dep.DependsOnID] = true
		}
	}
	if !synthBlockers[scopeID+".review-pipeline.review-claude-scope-check"] {
		t.Errorf("synthesize missing dep on review-claude scope-check")
	}
	if !synthBlockers[scopeID+".review-pipeline.review-codex-scope-check"] {
		t.Errorf("synthesize missing dep on review-codex scope-check")
	}

	// Apply-fixes blocks on synthesize scope-check.
	applyID := scopeID + ".apply-fixes"
	foundApplyDep := false
	for _, dep := range recipe.Deps {
		if dep.StepID == applyID && dep.DependsOnID == synthID+"-scope-check" && dep.Type == "blocks" {
			foundApplyDep = true
		}
	}
	if !foundApplyDep {
		t.Errorf("apply-fixes missing blocks dep on synthesize scope-check")
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

// ---------------------------------------------------------------------------
// findSpecBead: ref-preference disambiguation
// ---------------------------------------------------------------------------

func TestFindSpecBeadPrefersRefOverStepID(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()

	root := mustCreate(t, store, beads.Bead{
		Title: "workflow root",
		Metadata: map[string]string{
			"gc.kind": "workflow",
		},
	})

	// Two spec beads under the same root with the same gc.spec_for (logical
	// step ID) but different gc.spec_for_ref (namespaced). This happens when
	// a formula is instantiated multiple times in the same workflow.
	_ = mustCreate(t, store, beads.Bead{
		Title: "spec-old",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.spec_for":     "work",
			"gc.spec_for_ref": "mol.iteration.1.work",
			"gc.root_bead_id": root.ID,
		},
	})
	wantSpec := mustCreate(t, store, beads.Bead{
		Title: "spec-new",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.spec_for":     "work",
			"gc.spec_for_ref": "mol.iteration.2.work",
			"gc.root_bead_id": root.ID,
		},
	})

	control := mustCreate(t, store, beads.Bead{
		Title: "retry control",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.step_id":      "work",
			"gc.step_ref":     "mol.iteration.2.work",
			"gc.root_bead_id": root.ID,
		},
	})

	got, err := findSpecBead(store, control)
	if err != nil {
		t.Fatalf("findSpecBead: %v", err)
	}
	if got.ID != wantSpec.ID {
		t.Fatalf("findSpecBead returned %s (%s), want %s (%s)",
			got.ID, got.Title, wantSpec.ID, wantSpec.Title)
	}
}

// Unused import guard.
var _ = strconv.Itoa
