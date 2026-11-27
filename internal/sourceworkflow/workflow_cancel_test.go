package sourceworkflow

import (
	"errors"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// closeFailingStore fails any CloseAll batch that includes failOn, leaving
// the batch unwritten, and records every batch it sees.
type closeFailingStore struct {
	*beads.MemStore
	failOn  string
	batches [][]string
}

func (s *closeFailingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.batches = append(s.batches, slices.Clone(ids))
	if slices.Contains(ids, s.failOn) {
		return 0, errors.New("injected close failure")
	}
	return s.MemStore.CloseAll(ids, metadata)
}

// canceledRunTree is a run whose cancel stamped intent but did not wind down:
// the run root with a step, a teardown step and a drain control, and one drain
// item workflow nested under that control with its own step and teardown.
type canceledRunTree struct {
	run, step, teardown, drain, item, itemStep, itemTeardown beads.Bead
}

func seedCanceledRunTree(t *testing.T, store beads.Store) canceledRunTree {
	t.Helper()
	create := func(meta map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Title: "b", Type: "task", Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var tree canceledRunTree
	tree.run = create(map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.CancelRequestedMetadataKey: "true",
	})
	tree.step = create(map[string]string{beadmeta.RootBeadIDMetadataKey: tree.run.ID, beadmeta.StepIDMetadataKey: "report"})
	tree.teardown = create(map[string]string{
		beadmeta.RootBeadIDMetadataKey: tree.run.ID,
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleTeardown,
		beadmeta.StepIDMetadataKey:     "cleanup",
	})
	tree.drain = create(map[string]string{beadmeta.RootBeadIDMetadataKey: tree.run.ID, beadmeta.KindMetadataKey: beadmeta.KindDrain})
	tree.item = create(map[string]string{
		beadmeta.KindMetadataKey:           beadmeta.KindWorkflow,
		beadmeta.DrainControlIDMetadataKey: tree.drain.ID,
	})
	tree.itemStep = create(map[string]string{beadmeta.RootBeadIDMetadataKey: tree.item.ID, beadmeta.StepIDMetadataKey: "work"})
	tree.itemTeardown = create(map[string]string{
		beadmeta.RootBeadIDMetadataKey: tree.item.ID,
		beadmeta.ScopeRoleMetadataKey:  beadmeta.ScopeRoleTeardown,
		beadmeta.StepIDMetadataKey:     "item-cleanup",
	})
	return tree
}

func requireBeadState(t *testing.T, store beads.Store, id, status, outcome string) beads.Bead {
	t.Helper()
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	if got.Status != status || got.Metadata[beadmeta.OutcomeMetadataKey] != outcome {
		t.Fatalf("%s = %s/%q, want %s/%q", id, got.Status, got.Metadata[beadmeta.OutcomeMetadataKey], status, outcome)
	}
	return got
}

// TestCancelWorkflowTreeClosesNestedWorkflowsDeepestFirst: the recovery closes
// every open workflow of the run, the nested drain item before the run root,
// as canceled with the run-cancel reason and the cancel-intent marker on each
// root, and spares each workflow's teardown tail.
func TestCancelWorkflowTreeClosesNestedWorkflowsDeepestFirst(t *testing.T) {
	store := &closeFailingStore{MemStore: beads.NewMemStore()}
	tree := seedCanceledRunTree(t, store)

	if _, err := CancelWorkflowTree(beadstest.GraphMolecule(store), tree.run.ID); err != nil {
		t.Fatalf("CancelWorkflowTree: %v", err)
	}

	for _, id := range []string{tree.itemStep.ID, tree.item.ID, tree.step.ID, tree.drain.ID, tree.run.ID} {
		got := requireBeadState(t, store, id, "closed", beadmeta.OutcomeCanceled)
		if got.Metadata["close_reason"] != RunCanceledCloseReason {
			t.Fatalf("%s close_reason = %q, want %q", id, got.Metadata["close_reason"], RunCanceledCloseReason)
		}
	}
	for _, id := range []string{tree.run.ID, tree.item.ID} {
		if got, _ := store.Get(id); got.Metadata[beadmeta.CancelRequestedMetadataKey] != "true" {
			t.Fatalf("%s gc.cancel_requested = %q, want true", id, got.Metadata[beadmeta.CancelRequestedMetadataKey])
		}
	}
	requireBeadState(t, store, tree.teardown.ID, "open", "")
	requireBeadState(t, store, tree.itemTeardown.ID, "open", "")

	itemClosed, runClosed := -1, -1
	for i, batch := range store.batches {
		if slices.Contains(batch, tree.item.ID) {
			itemClosed = i
		}
		if slices.Contains(batch, tree.run.ID) {
			runClosed = i
		}
	}
	if itemClosed < 0 || runClosed < 0 || itemClosed > runClosed {
		t.Fatalf("close batches = %v, want item root %s closed before run root %s", store.batches, tree.item.ID, tree.run.ID)
	}
}

// TestCancelWorkflowTreeStopsAtFirstError: a close that fails on a nested
// workflow ends the sweep there, so the run root and its own members stay open
// for a retried cancel to find.
func TestCancelWorkflowTreeStopsAtFirstError(t *testing.T) {
	store := &closeFailingStore{MemStore: beads.NewMemStore()}
	tree := seedCanceledRunTree(t, store)
	store.failOn = tree.item.ID

	if _, err := CancelWorkflowTree(beadstest.GraphMolecule(store), tree.run.ID); err == nil {
		t.Fatal("CancelWorkflowTree succeeded, want the injected close failure")
	}

	for _, id := range []string{tree.run.ID, tree.step.ID, tree.drain.ID, tree.item.ID} {
		requireBeadState(t, store, id, "open", "")
	}
}
