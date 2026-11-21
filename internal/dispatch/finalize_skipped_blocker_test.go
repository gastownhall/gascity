package dispatch

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// skippedBlockerWorkflowFixture builds an open workflow root and its
// workflow-finalize control, blocked by one closed step per entry of
// outcomes (the step's gc.outcome). The edges are added in reverse creation
// order, so a marker that kept edge order instead of sorting reads wrong.
func skippedBlockerWorkflowFixture(t *testing.T, store beads.Store, outcomes ...string) (beads.Bead, beads.Bead, []beads.Bead) {
	t.Helper()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	steps := make([]beads.Bead, 0, len(outcomes))
	for _, outcome := range outcomes {
		steps = append(steps, mustCreateWorkflowBead(t, store, beads.Bead{
			Title:    "step",
			Type:     "task",
			Status:   "closed",
			Metadata: map[string]string{"gc.outcome": outcome},
		}))
	}
	finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	for i := len(steps) - 1; i >= 0; i-- {
		mustDepAdd(t, store, finalizer.ID, steps[i].ID, "blocks")
	}
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")
	return root, finalizer, steps
}

func finalizeSkippedBlockerWorkflow(t *testing.T, store beads.Store, finalizer beads.Bead, wantAction string) {
	t.Helper()
	result, err := ProcessControl(beadstest.Graph(store), finalizer, ProcessOptions{Work: beadstest.Work(store)})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !result.Processed || result.Action != wantAction {
		t.Fatalf("result = %+v, want processed %s", result, wantAction)
	}
}

// gastownhall/gascity#6096: a skipped blocker is not a failure, so the
// workflow still passes, but its root must say which blockers never ran. A
// root that read only gc.outcome=pass could not tell a step skipped by a
// force-close from a step that completed.
func TestProcessWorkflowFinalizeMarksSkippedBlockersOnPass(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, finalizer, steps := skippedBlockerWorkflowFixture(t, store, "skipped", "pass", "skipped")
	finalizeSkippedBlockerWorkflow(t, store, finalizer, "workflow-pass")

	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = %s/%q, want closed/pass (a skip is not a failure)", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	skipped := []string{steps[0].ID, steps[2].ID}
	slices.Sort(skipped)
	want := strings.Join(skipped, ",")
	if got := rootAfter.Metadata["gc.skipped_blockers"]; got != want {
		t.Fatalf("root gc.skipped_blockers = %q, want %q (the skipped blockers, sorted; the passed one left out)", got, want)
	}
}

// A pass with no skipped blocker writes no marker, so the key's presence
// alone means "a blocker was skipped".
func TestProcessWorkflowFinalizeAllPassedWritesNoSkipMarker(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, finalizer, _ := skippedBlockerWorkflowFixture(t, store, "pass", "pass")
	finalizeSkippedBlockerWorkflow(t, store, finalizer, "workflow-pass")

	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got, ok := rootAfter.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("root gc.skipped_blockers = %q, want the key absent when every blocker passed", got)
	}
}

// The marker qualifies a pass only. A failed workflow already names its
// culprit, and a skipped sibling of the failed step adds nothing to it.
func TestProcessWorkflowFinalizeFailWritesNoSkipMarker(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	root, finalizer, _ := skippedBlockerWorkflowFixture(t, store, "skipped", "fail")
	finalizeSkippedBlockerWorkflow(t, store, finalizer, "workflow-fail")

	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got := rootAfter.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("root gc.outcome = %q, want fail", got)
	}
	if got, ok := rootAfter.Metadata["gc.skipped_blockers"]; ok {
		t.Fatalf("root gc.skipped_blockers = %q, want the key absent on a failed workflow", got)
	}
}
