package main

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// orphanShape builds a zero-progress orphan molecule matching the live shape
// from crn-vbsr0: a molecule root, one target bead attached via
// metadata.molecule_id and already closed, and stepCount untouched step
// descendants parented under the root.
func orphanShape(t *testing.T, store beads.Store, stepCount int) (rootID string, stepIDs []string) {
	t.Helper()
	root, err := store.Create(beads.Bead{Title: "mol-tdd-build", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Create(beads.Bead{
		Title:    "target work item",
		Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(target.ID); err != nil {
		t.Fatal(err)
	}
	steps := make([]string, 0, stepCount)
	for i := 0; i < stepCount; i++ {
		s, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("step-%d", i),
			Type:     "step",
			ParentID: root.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, s.ID)
	}
	return root.ID, steps
}

// TestReapOrphans_ZeroProgressOrphanIsClosed pins the primary fix: predicate
// 1 (root open) + 2 (sole target bead terminal) + 3a (every descendant
// untouched since creation) closes the whole subtree. Also pins Done-when's
// idempotency requirement: a second run finds nothing left to reap.
func TestReapOrphans_ZeroProgressOrphanIsClosed(t *testing.T) {
	store := beads.NewMemStore()
	rootID, stepIDs := orphanShape(t, store, 3)

	var out bytes.Buffer
	results, err := reapOrphanMolecules(store, reapOrphansOptions{}, &out)
	if err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(results) != 1 || results[0].RootID != rootID {
		t.Fatalf("results = %+v, want single result for %s", results, rootID)
	}

	root, err := store.Get(rootID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed", root.Status)
	}
	for _, id := range stepIDs {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "closed" {
			t.Fatalf("step %s status = %q, want closed", id, b.Status)
		}
	}

	out.Reset()
	results2, err := reapOrphanMolecules(store, reapOrphansOptions{}, &out)
	if err != nil {
		t.Fatalf("second reapOrphanMolecules: %v", err)
	}
	if len(results2) != 0 {
		t.Fatalf("second run results = %+v, want none (root already closed, no longer a candidate)", results2)
	}
}

// TestReapOrphans_DryRunClosesNothing pins Done-when's "dry-run first"
// requirement: dry-run reports what would be reaped without closing anything.
func TestReapOrphans_DryRunClosesNothing(t *testing.T) {
	store := beads.NewMemStore()
	rootID, stepIDs := orphanShape(t, store, 2)

	var out bytes.Buffer
	results, err := reapOrphanMolecules(store, reapOrphansOptions{DryRun: true}, &out)
	if err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(results) != 1 || !results[0].DryRun {
		t.Fatalf("results = %+v, want one dry-run result", results)
	}

	root, err := store.Get(rootID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "open" {
		t.Fatalf("dry-run must not close root, status = %q", root.Status)
	}
	for _, id := range stepIDs {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "open" {
			t.Fatalf("dry-run must not close step %s, status = %q", id, b.Status)
		}
	}
}

// TestReapOrphans_TouchedOpenStepIsNotReaped is the Done-when-required
// negative test: "a molecule with ANY touched-but-open step is NOT reaped
// under 3a." Target-closed-with-open-steps is exactly the #3474 parked-gate
// shape; only the never-entered (created_at == updated_at) signal makes
// closing here safe, so a single touched step must veto the whole subtree.
func TestReapOrphans_TouchedOpenStepIsNotReaped(t *testing.T) {
	store := beads.NewMemStore()
	rootID, stepIDs := orphanShape(t, store, 2)

	touched := stepIDs[0]
	if err := store.Update(touched, beads.UpdateOpts{Assignee: strPtr("someone")}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	results, err := reapOrphanMolecules(store, reapOrphansOptions{}, &out)
	if err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none (touched step must block 3a reap)", results)
	}

	root, err := store.Get(rootID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != "open" {
		t.Fatalf("root status = %q, want open (parked/touched subtree preserved)", root.Status)
	}
	for _, id := range stepIDs {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "open" {
			t.Fatalf("step %s status = %q, want open (parked/touched subtree preserved)", id, b.Status)
		}
	}
}

// TestReapOrphans_NoTargetBeadIsNotReaped guards against a vacuous-truth bug
// in predicate 2 ("every target bead is terminal"): zero target beads must
// NOT satisfy that check. Without this, a molecule that simply has not been
// slung to a target yet would be reaped out from under it.
func TestReapOrphans_NoTargetBeadIsNotReaped(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "mol-tdd-build", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	results, err := reapOrphanMolecules(store, reapOrphansOptions{}, &out)
	if err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none (no target bead means not an orphan)", results)
	}
	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" {
		t.Fatalf("root status = %q, want open", got.Status)
	}
}

// TestReapOrphans_OpenTargetIsNotReaped pins predicate 2's "every target bead
// is terminal" half: a molecule still slung to an open target is genuinely
// in flight, not orphaned.
func TestReapOrphans_OpenTargetIsNotReaped(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "mol-tdd-build", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "target",
		Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID},
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	results, err := reapOrphanMolecules(store, reapOrphansOptions{}, &out)
	if err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none (open target means still in flight)", results)
	}
}
