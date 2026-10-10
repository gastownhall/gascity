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
	meta := controlQuarantinedRootMetadata()
	meta["gc.kind"] = "workflow"
	b := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "quarantined",
		Type:     "task",
		Status:   "closed",
		Metadata: meta,
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

// The helper's guard is scoped to workflow ROOTS. A retry/ralph logical bead
// that was quarantined and later passed keeps its own terminal semantics
// (review of #162, S1): the pass is written, not silently dropped.
func TestSetOutcomeAndCloseStillPassesAQuarantinedNonRoot(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	meta := controlQuarantinedRootMetadata()
	meta["gc.kind"] = "retry"
	b := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "logical retry bead",
		Type:     "task",
		Status:   "closed",
		Metadata: meta,
	})
	if err := setOutcomeAndClose(store, b.ID, "pass"); err != nil {
		t.Fatalf("setOutcomeAndClose: %v", err)
	}
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := after.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("non-root gc.outcome = %q, want pass (the guard is for workflow roots only)", got)
	}
}

// The incident's second harm: the pass verdict closed the root's source chain
// as passed. A quarantined root must leave its sources open and carry the
// root's own diagnosis to its direct source (review of #162, S2/S4).
func TestProcessWorkflowFinalizeQuarantinedRootLeavesSourcesOpenWithItsDiagnosis(t *testing.T) {
	t.Parallel()

	f := newSourceChainFinalizeFixture(t)
	for k, v := range controlQuarantinedRootMetadata() {
		if err := f.rigStore.SetMetadata(f.workflow.ID, k, v); err != nil {
			t.Fatalf("SetMetadata(%s): %v", k, err)
		}
	}
	if err := f.rigStore.Close(f.workflow.ID); err != nil {
		t.Fatalf("close root: %v", err)
	}

	result, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{ResolveStoreRef: f.resolver})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("result = %+v, want processed workflow-fail", result)
	}
	launch, err := f.rigStore.Get(f.rigLaunch.ID)
	if err != nil {
		t.Fatalf("get rig launch: %v", err)
	}
	if launch.Status == "closed" {
		t.Fatalf("rig launch source was closed; a quarantined root must leave its sources open")
	}
	if got := launch.Metadata["gc.failure_reason"]; got != "control_dispatch_error" {
		t.Fatalf("source gc.failure_reason = %q, want the root's control_dispatch_error", got)
	}
	if got := launch.Metadata["gc.failure_class"]; got != "hard" {
		t.Fatalf("source gc.failure_class = %q, want hard", got)
	}
	city, err := f.cityStore.Get(f.citySource.ID)
	if err != nil {
		t.Fatalf("get city source: %v", err)
	}
	if city.Status == "closed" {
		t.Fatalf("city source was closed; a quarantined root must leave the chain open")
	}
}
