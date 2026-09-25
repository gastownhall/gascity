package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// quarantinedWorkflowFixture builds a workflow root that the control
// dispatcher has already quarantined as a hard failure (the metadata
// quarantineControlFailureBead in cmd/gc writes), plus its still-open
// workflow-finalize control whose only blocker PASSED. That is the shape of
// the 2026-09-25 incident (ga-3wlbcj): 61 roots quarantined with
// `unsupported control bead kind "workflow"` later read gc.outcome=pass,
// because the finalizer recomputed the outcome from its blockers and merged
// it onto the closed root.
func quarantinedWorkflowFixture(t *testing.T, rootStatus string, rootMeta map[string]string) (beads.Store, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	meta := map[string]string{
		"gc.kind":             "workflow",
		"gc.formula_contract": "graph.v2",
	}
	for k, v := range rootMeta {
		meta[k] = v
	}
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Status:   rootStatus,
		Metadata: meta,
	})
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "step",
		Type:     "task",
		Status:   "closed",
		Metadata: map[string]string{"gc.outcome": "pass"},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	mustDepAdd(t, store, finalizer.ID, step.ID, "blocks")
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")
	return store, root, finalizer
}

func controlQuarantinedRootMetadata() map[string]string {
	return map[string]string{
		"gc.outcome":                "fail",
		"gc.failure_class":          "hard",
		"gc.failure_reason":         "control_dispatch_error",
		"gc.final_disposition":      "control_quarantined",
		"gc.control_quarantined":    "true",
		"gc.controller_error":       `unsupported control bead kind "workflow"`,
		"gc.controller_error_class": "hard",
	}
}

func TestProcessWorkflowFinalizeKeepsQuarantinedRootFailed(t *testing.T) {
	t.Parallel()

	store, root, finalizer := quarantinedWorkflowFixture(t, "closed", controlQuarantinedRootMetadata())

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("result = %+v, want processed workflow-fail (a quarantined root is a failed workflow)", result)
	}
	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got := rootAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("quarantined root gc.outcome = %q, want fail", got)
	}
	if got := rootAfter.Metadata["gc.final_disposition"]; got != "control_quarantined" {
		t.Fatalf("root gc.final_disposition = %q, want control_quarantined preserved", got)
	}
	finalizerAfter, err := store.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if finalizerAfter.Status != "closed" {
		t.Fatalf("finalizer status = %q, want closed (it must still drain)", finalizerAfter.Status)
	}
}

// The negative case: an open root whose blockers all passed still finalizes
// green. The guard must key on the quarantine, never on "root already has
// metadata".
func TestProcessWorkflowFinalizePassedRootStillReadsPass(t *testing.T) {
	t.Parallel()

	store, root, finalizer := quarantinedWorkflowFixture(t, "open", nil)

	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass", result)
	}
	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = %s/%q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
}

// The shared close helper is the last line: whatever caller computes pass,
// it must not merge pass over a closed, control-quarantined bead.
func TestSetOutcomeAndCloseNeverPassesOverQuarantine(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	b := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "quarantined",
		Type:     "task",
		Status:   "closed",
		Metadata: controlQuarantinedRootMetadata(),
	})
	if err := setOutcomeAndClose(store, b.ID, "pass"); err != nil {
		t.Fatalf("setOutcomeAndClose: %v", err)
	}
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := after.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("gc.outcome = %q, want fail kept over a pass write", got)
	}
}
