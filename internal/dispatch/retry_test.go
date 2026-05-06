package dispatch

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestProcessRetryEvalPassClosesLogical(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "pass",
			"gc.output_json":     `{"ok":true}`,
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(store, eval1, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval pass): %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	evalAfter := mustGetBead(t, store, eval1.ID)
	if evalAfter.Status != "closed" || evalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("eval = status %q outcome %q, want closed/pass", evalAfter.Status, evalAfter.Metadata["gc.outcome"])
	}
	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("logical = status %q outcome %q, want closed/pass", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
	if logicalAfter.Metadata["gc.final_disposition"] != "pass" {
		t.Fatalf("logical gc.final_disposition = %q, want pass", logicalAfter.Metadata["gc.final_disposition"])
	}
	if logicalAfter.Metadata["gc.output_json"] != `{"ok":true}` {
		t.Fatalf("logical gc.output_json = %q, want propagated output", logicalAfter.Metadata["gc.output_json"])
	}
}

func TestProcessRetryEvalRetriesPassMissingRequiredOutputJSON(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "prepare review items",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                 "retry",
			"gc.root_bead_id":         root.ID,
			"gc.step_ref":             "demo.prepare-review-items",
			"gc.max_attempts":         "3",
			"gc.on_exhausted":         "hard_fail",
			"gc.output_json_required": "true",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "prepare review items attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":                 "retry-run",
			"gc.root_bead_id":         root.ID,
			"gc.step_ref":             "demo.prepare-review-items.run.1",
			"gc.logical_bead_id":      logical.ID,
			"gc.attempt":              "1",
			"gc.max_attempts":         "3",
			"gc.on_exhausted":         "hard_fail",
			"gc.outcome":              "pass",
			"gc.output_json_required": "true",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "prepare review items eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.prepare-review-items.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(store, eval1, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval missing output_json): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}

	evalAfter := mustGetBead(t, store, eval1.ID)
	if evalAfter.Status != "closed" || evalAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("eval = status %q outcome %q, want closed/fail", evalAfter.Status, evalAfter.Metadata["gc.outcome"])
	}
	if evalAfter.Metadata["gc.failure_class"] != "transient" {
		t.Fatalf("eval gc.failure_class = %q, want transient", evalAfter.Metadata["gc.failure_class"])
	}
	if evalAfter.Metadata["gc.failure_reason"] != "missing_required_output_json" {
		t.Fatalf("eval gc.failure_reason = %q, want missing_required_output_json", evalAfter.Metadata["gc.failure_reason"])
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "open" {
		t.Fatalf("logical status = %q, want open", logicalAfter.Status)
	}

	var run2 beads.Bead
	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen(): %v", err)
	}
	for _, bead := range all {
		if bead.Metadata["gc.step_ref"] == "demo.prepare-review-items.run.2" {
			run2 = bead
		}
	}
	if run2.ID == "" {
		t.Fatal("missing retry run 2")
	}
}

func TestProcessRetryEvalPassPropagatesNonGCMetadataToLogical(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply fixes",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.apply-fixes",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply fixes attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.apply-fixes.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "pass",
			"review.verdict":     "done",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply fixes eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.apply-fixes.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(store, eval1, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval pass verdict propagation): %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if got := logicalAfter.Metadata["review.verdict"]; got != "done" {
		t.Fatalf("logical review.verdict = %q, want done", got)
	}
}

func TestProcessRetryEvalPassUsesRetryRunInsteadOfBlockingControlDeps(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply fixes",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.apply-fixes",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "apply fixes attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.apply-fixes.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "pass",
			"review.verdict":     "done",
		},
	})
	runScopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "Finalize apply fixes attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.control_for":  "apply-fixes.run.1",
			"gc.outcome":      "pass",
		},
	})
	unrelatedScopeCheck := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "Finalize unrelated scope",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.control_for":  "other-step",
			"gc.outcome":      "pass",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "apply fixes eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.apply-fixes.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, runScopeCheck.ID, run1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, runScopeCheck.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, unrelatedScopeCheck.ID, "blocks")
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")

	result, err := ProcessControl(store, eval1, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval live-style deps): %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if got := logicalAfter.Metadata["review.verdict"]; got != "done" {
		t.Fatalf("logical review.verdict = %q, want done", got)
	}
}

func TestProcessRetryEvalResolvesLogicalByStepRefFallback(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "gemini review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review-loop.run.1.review-pipeline.review-gemini",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "soft_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "gemini review attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":         "retry-run",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review-loop.run.1.review-pipeline.review-gemini.run.1",
			"gc.attempt":      "1",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "soft_fail",
			"gc.outcome":      "pass",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "gemini review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry-eval",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review-loop.run.1.review-pipeline.review-gemini.eval.1",
			"gc.attempt":      "1",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "soft_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(store, eval1, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval fallback pass): %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("logical = status %q outcome %q, want closed/pass", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
}

func TestProcessRetryEvalTransientRetriesAndRecyclesPoolSession(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "review attempt 1",
		Type:     "task",
		Status:   "closed",
		Assignee: "polecat-2",
		Labels:   []string{"pool:polecat"},
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "fail",
			"gc.failure_class":   "transient",
			"gc.failure_reason":  "rate_limited",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	var recycled []string
	result, err := ProcessControl(store, eval1, ProcessOptions{
		RecycleSession: func(subject beads.Bead) error {
			recycled = append(recycled, subject.Assignee)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval transient): %v", err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("result = %+v, want processed retry", result)
	}
	if len(recycled) != 1 || recycled[0] != "polecat-2" {
		t.Fatalf("recycled = %v, want [polecat-2]", recycled)
	}

	evalAfter := mustGetBead(t, store, eval1.ID)
	if evalAfter.Status != "closed" || evalAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("eval = status %q outcome %q, want closed/fail", evalAfter.Status, evalAfter.Metadata["gc.outcome"])
	}
	if evalAfter.Metadata["gc.retry_session_recycled"] != "true" {
		t.Fatalf("eval gc.retry_session_recycled = %q, want true", evalAfter.Metadata["gc.retry_session_recycled"])
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "open" {
		t.Fatalf("logical status = %q, want open", logicalAfter.Status)
	}

	var run2, eval2 beads.Bead
	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	for _, bead := range all {
		switch bead.Metadata["gc.step_ref"] {
		case "demo.review.run.2":
			run2 = bead
		case "demo.review.eval.2":
			eval2 = bead
		}
	}
	if run2.ID == "" || eval2.ID == "" {
		t.Fatalf("missing retry attempt beads: run2=%q eval2=%q", run2.ID, eval2.ID)
	}
	if run2.Assignee != "" {
		t.Fatalf("run2 assignee = %q, want empty for pooled retry", run2.Assignee)
	}
	if got := run2.Metadata["gc.retry_from"]; got != run1.ID {
		t.Fatalf("run2 gc.retry_from = %q, want %s", got, run1.ID)
	}
	if got := eval2.Metadata["gc.retry_from"]; got != eval1.ID {
		t.Fatalf("eval2 gc.retry_from = %q, want %s", got, eval1.ID)
	}
	logicalDeps, err := store.DepList(logical.ID, "down")
	if err != nil {
		t.Fatalf("logical deps: %v", err)
	}
	if len(logicalDeps) != 1 || logicalDeps[0].DependsOnID != eval2.ID {
		t.Fatalf("logical deps = %+v, want only current retry eval %s", logicalDeps, eval2.ID)
	}
}

func TestProcessRetryEvalSoftFailOnExhaustedTransient(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "gemini review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review-gemini",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "soft_fail",
		},
	})
	run3 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "gemini review attempt 3",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review-gemini.run.3",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "3",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "soft_fail",
			"gc.outcome":         "fail",
			"gc.failure_class":   "transient",
			"gc.failure_reason":  "rate_limited",
		},
	})
	eval3 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "gemini review eval 3",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review-gemini.eval.3",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "3",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "soft_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval3.ID, "blocks")
	mustDepAdd(t, store, eval3.ID, run3.ID, "blocks")

	result, err := ProcessControl(store, eval3, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval soft-fail): %v", err)
	}
	if !result.Processed || result.Action != "soft-fail" {
		t.Fatalf("result = %+v, want processed soft-fail", result)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("logical = status %q outcome %q, want closed/pass", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
	if logicalAfter.Metadata["gc.final_disposition"] != "soft_fail" {
		t.Fatalf("logical gc.final_disposition = %q, want soft_fail", logicalAfter.Metadata["gc.final_disposition"])
	}
	if logicalAfter.Metadata["gc.failure_reason"] != "rate_limited" {
		t.Fatalf("logical gc.failure_reason = %q, want rate_limited", logicalAfter.Metadata["gc.failure_reason"])
	}
}

func TestProcessRetryEvalStaleAttemptFinalizesNoop(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":              "retry",
			"gc.root_bead_id":      root.ID,
			"gc.step_ref":          "demo.review",
			"gc.max_attempts":      "3",
			"gc.on_exhausted":      "hard_fail",
			"gc.closed_by_attempt": "2",
			"gc.outcome":           "pass",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "fail",
			"gc.failure_class":   "transient",
			"gc.failure_reason":  "rate_limited",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.retry_state":     "spawned",
			"gc.next_attempt":    "2",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(store, eval1, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval stale noop): %v", err)
	}
	if !result.Processed || result.Action != "noop" {
		t.Fatalf("result = %+v, want processed noop", result)
	}

	evalAfter := mustGetBead(t, store, eval1.ID)
	if evalAfter.Status != "closed" || evalAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("eval = status %q outcome %q, want closed/fail", evalAfter.Status, evalAfter.Metadata["gc.outcome"])
	}
}

func TestProcessRetryEvalRejectsInvalidWorkerResultContract(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "2",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "2",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "2",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(store, eval1, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval invalid contract): %v", err)
	}
	if !result.Processed || result.Action != "hard-fail" {
		t.Fatalf("result = %+v, want processed hard-fail", result)
	}

	logicalAfter := mustGetBead(t, store, logical.ID)
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("logical = status %q outcome %q, want closed/fail", logicalAfter.Status, logicalAfter.Metadata["gc.outcome"])
	}
	if !strings.Contains(logicalAfter.Metadata["gc.failure_reason"], "invalid_worker_result_contract") {
		t.Fatalf("logical gc.failure_reason = %q, want invalid_worker_result_contract", logicalAfter.Metadata["gc.failure_reason"])
	}
}

func TestProcessScopeCheckSkipsOpenRetryDescendantsOnAbort(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.body",
			"gc.scope_role":   "body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_ref":     "demo.review",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review attempt 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry-run",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review.run.1",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry-eval",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review.eval.1",
		},
	})
	mustDepAdd(t, store, control.ID, failed.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check with retry descendants): %v", err)
	}
	if !result.Processed || result.Action != "scope-fail" {
		t.Fatalf("result = %+v, want processed scope-fail", result)
	}

	for _, beadID := range []string{logical.ID, run1.ID, eval1.ID} {
		bead := mustGetBead(t, store, beadID)
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", beadID, bead.Status)
		}
		if bead.Metadata["gc.outcome"] != "skipped" {
			t.Fatalf("%s gc.outcome = %q, want skipped", beadID, bead.Metadata["gc.outcome"])
		}
	}
}

func TestProcessScopeCheckSkipsOpenRalphIterationDescendantsOnAbort(t *testing.T) {
	t.Parallel()

	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	body := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "body",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.body",
			"gc.scope_role":   "body",
		},
	})
	failed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "preflight",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.outcome":      "fail",
		},
	})
	control := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for preflight",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope-check",
			"gc.root_bead_id": root.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "control",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review loop",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.scope_ref":    "body",
			"gc.scope_role":   "member",
			"gc.step_id":      "review-loop",
			"gc.step_ref":     "demo.review-loop",
		},
	})
	iterationChild := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":          "retry",
			"gc.root_bead_id":  root.ID,
			"gc.scope_ref":     "review-loop.iteration.1",
			"gc.scope_role":    "member",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "1",
			"gc.step_ref":      "demo.review-loop.iteration.1.review-claude",
		},
	})
	iterationControl := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize scope for review claude",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":          "scope-check",
			"gc.root_bead_id":  root.ID,
			"gc.scope_ref":     "review-loop.iteration.1",
			"gc.scope_role":    "control",
			"gc.ralph_step_id": "review-loop",
			"gc.attempt":       "1",
			"gc.step_ref":      "demo.review-loop.iteration.1.review-claude-scope-check",
		},
	})
	mustDepAdd(t, store, control.ID, failed.ID, "blocks")
	mustDepAdd(t, store, body.ID, control.ID, "blocks")
	mustDepAdd(t, store, logical.ID, iterationControl.ID, "blocks")
	mustDepAdd(t, store, iterationControl.ID, iterationChild.ID, "blocks")

	result, err := ProcessControl(store, control, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(scope-check with ralph descendants): %v", err)
	}
	if !result.Processed || result.Action != "scope-fail" {
		t.Fatalf("result = %+v, want processed scope-fail", result)
	}

	for _, beadID := range []string{logical.ID, iterationChild.ID, iterationControl.ID} {
		bead := mustGetBead(t, store, beadID)
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", beadID, bead.Status)
		}
		if bead.Metadata["gc.outcome"] != "skipped" {
			t.Fatalf("%s gc.outcome = %q, want skipped", beadID, bead.Metadata["gc.outcome"])
		}
	}
}
