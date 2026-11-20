package dispatch

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// A workflow-finalize whose direct blocks blockers include steps closed
// gc.outcome=skipped still passes (a skip is deliberately not a failure), but
// the root must say so: gc.skipped_blockers lists the skipped blockers' IDs,
// sorted and comma-joined, so a pass earned with skipped work reads
// differently from one whose every blocker ran (#6096).
func TestProcessWorkflowFinalizeMarksSkippedBlockersOnPass(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	passed := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "ran", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "pass"},
	})
	skippedA := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "skipped a", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "skipped"},
	})
	skippedB := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "skipped b", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "skipped"},
	})
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow", Type: "task",
		Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": root.ID},
	})
	// Edges added in descending-blocker order: MemStore returns dependencies
	// in insertion order, so the skipped IDs reach the scan unsorted.
	mustDepAdd(t, store, finalizer.ID, skippedB.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, passed.ID, "blocks")
	mustDepAdd(t, store, finalizer.ID, skippedA.ID, "blocks")
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")

	// Precondition: the skipped blockers really do arrive unsorted
	// (skippedB before skippedA), so the assertion below tests the marker's
	// sort, not the store's edge ordering.
	deps, err := store.DepList(finalizer.ID, "down")
	if err != nil {
		t.Fatalf("dep list: %v", err)
	}
	var depOrder []string
	for _, dep := range deps {
		if dep.Type != "blocks" {
			continue
		}
		if dep.DependsOnID == skippedA.ID || dep.DependsOnID == skippedB.ID {
			depOrder = append(depOrder, dep.DependsOnID)
		}
	}
	if slices.IsSorted(depOrder) {
		t.Fatalf("precondition: skipped blockers' dependency-list order %v is sorted; add the edges in an order that sorts the other way", depOrder)
	}

	result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-pass" {
		t.Fatalf("result = %+v, want processed workflow-pass (a skip is not a failure)", result)
	}
	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = %s/%q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	want := []string{skippedA.ID, skippedB.ID}
	slices.Sort(want)
	if got := rootAfter.Metadata["gc.skipped_blockers"]; got != strings.Join(want, ",") {
		t.Fatalf("root gc.skipped_blockers = %q, want %q (a pass with skipped blockers must be marked)", got, strings.Join(want, ","))
	}
}
