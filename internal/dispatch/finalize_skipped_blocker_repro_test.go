package dispatch

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// finalizeWithNativeStep finalizes a workflow whose finalizer is blocked by a
// single native step closed with stepMeta, and returns the closed root.
func finalizeWithNativeStep(t *testing.T, stepMeta map[string]string) beads.Bead {
	t.Helper()
	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	meta := map[string]string{
		"gc.root_bead_id": workflow.ID,
		"gc.step_ref":     "build-from-plan.publish",
	}
	maps.Copy(meta, stepMeta)
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "publish",
		Type:     "task",
		Status:   "closed",
		Metadata: meta,
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": workflow.ID,
		},
	})
	mustDepAdd(t, store, finalizer.ID, step.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	if _, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	root := mustGetBead(t, store, workflow.ID)
	if root.Status != "closed" {
		t.Fatalf("workflow status = %q, want closed", root.Status)
	}
	return root
}

// TestProcessWorkflowFinalizeSkippedNativeStepIsDistinguishableFromPass
// reproduces gastownhall/gascity#6096: a native step dependency force-closed
// via the skip directive (gc.outcome=skipped) must not finalize the workflow
// root in a state identical to one whose step actually completed. Either the
// outcome differs or the root carries a distinguishing marker.
func TestProcessWorkflowFinalizeSkippedNativeStepIsDistinguishableFromPass(t *testing.T) {
	t.Parallel()

	passed := finalizeWithNativeStep(t, map[string]string{
		"gc.outcome": "pass",
	})
	skipped := finalizeWithNativeStep(t, map[string]string{
		"gc.outcome":   "skipped",
		"close_reason": sourceworkflow.WorkflowSkippedCloseReason,
	})

	if maps.Equal(passed.Metadata, skipped.Metadata) {
		t.Fatalf("workflow root after a skip-directive-closed native step is indistinguishable from a real pass: both closed with metadata %v", skipped.Metadata)
	}
}
