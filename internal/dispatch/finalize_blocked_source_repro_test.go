package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestProcessWorkflowFinalizeStopsSourceChainAtBlockedSource asserts that a
// passing workflow whose source bead recorded gc.work_outcome=blocked closes
// that source in its blocked form but leaves the source's own parent open:
// blocked work did not complete the parent.
func TestProcessWorkflowFinalizeStopsSourceChainAtBlockedSource(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	parent := mustCreateWorkflowBead(t, store, beads.Bead{Title: "Parent request", Type: "task"})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Blocked work item",
		Type:  "task",
		Metadata: map[string]string{
			"gc.source_bead_id": parent.ID,
			"gc.work_outcome":   "blocked",
		},
	})
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "mol-do-work",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	step := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "implement",
		Type:     "task",
		Status:   "closed",
		Metadata: map[string]string{"gc.outcome": "pass", "gc.root_bead_id": workflow.ID},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "Finalize workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": workflow.ID},
	})
	mustDepAdd(t, store, finalizer.ID, step.ID, "blocks")
	mustDepAdd(t, store, workflow.ID, finalizer.ID, "blocks")

	if _, err := ProcessControl(store, finalizer, ProcessOptions{}); err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	sourceAfter, err := store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter.Status != "closed" || sourceAfter.Metadata["gc.work_outcome"] != "blocked" {
		t.Fatalf("blocked source = status %q work_outcome %q, want closed blocked", sourceAfter.Status, sourceAfter.Metadata["gc.work_outcome"])
	}
	parentAfter, err := store.Get(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parentAfter.Status == "closed" {
		t.Fatalf("parent of blocked source closed with gc.outcome %q; the source chain must stop at a gc.work_outcome=blocked source", parentAfter.Metadata["gc.outcome"])
	}
}
