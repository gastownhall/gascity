package main

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
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
	results, err := reapOrphanMolecules(store, events.Discard, reapOrphansOptions{}, &out)
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
	results2, err := reapOrphanMolecules(store, events.Discard, reapOrphansOptions{}, &out)
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
	results, err := reapOrphanMolecules(store, events.Discard, reapOrphansOptions{DryRun: true}, &out)
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
	results, err := reapOrphanMolecules(store, events.Discard, reapOrphansOptions{}, &out)
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
	results, err := reapOrphanMolecules(store, events.Discard, reapOrphansOptions{}, &out)
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
	results, err := reapOrphanMolecules(store, events.Discard, reapOrphansOptions{}, &out)
	if err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %+v, want none (open target means still in flight)", results)
	}
}

// TestReapOrphans_PublishesClosedAndResolvedEvents pins layering invariant #3:
// a reaped molecule root is announced on the event bus with the same
// bead.closed / molecule.resolved pair the autoclose path emits
// (announceClosedMolecule), carrying the session attribution stamped on the
// root and the reap close_reason.
func TestReapOrphans_PublishesClosedAndResolvedEvents(t *testing.T) {
	store := beads.NewMemStore()
	rootID, _ := orphanShape(t, store, 3)
	if err := store.SetMetadataBatch(rootID, map[string]string{
		beadmeta.SessionNameMetadataKey: "polecat-gc-42",
		beadmeta.SessionIDMetadataKey:   "gc-42",
		beadmeta.WorkDirMetadataKey:     "/home/ds/gascity-worktrees/polecat-1",
	}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var out bytes.Buffer
	if _, err := reapOrphanMolecules(store, rec, reapOrphansOptions{}, &out); err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}

	closed := eventsOfType(rec.Events, events.BeadClosed)
	if len(closed) != 1 || closed[0].Subject != rootID {
		t.Fatalf("bead.closed events = %+v, want exactly one for root %s", closed, rootID)
	}
	if closed[0].Actor == "" {
		t.Errorf("bead.closed Actor empty, want eventActor() identity")
	}

	resolved := eventsOfType(rec.Events, events.MoleculeResolved)
	if len(resolved) != 1 || resolved[0].Subject != rootID {
		t.Fatalf("molecule.resolved events = %+v, want exactly one for root %s", resolved, rootID)
	}
	p := decodeMoleculeResolvedPayload(t, resolved[0])
	if p.IssueID != rootID {
		t.Errorf("IssueID = %q, want %q", p.IssueID, rootID)
	}
	if p.FromStatus != "open" || p.ToStatus != "closed" {
		t.Errorf("status transition = %q -> %q, want open -> closed", p.FromStatus, p.ToStatus)
	}
	if p.CloseReason != reapOrphanCloseReason {
		t.Errorf("CloseReason = %q, want %q", p.CloseReason, reapOrphanCloseReason)
	}
	if p.SessionName != "polecat-gc-42" || p.SessionID != "gc-42" || p.WorkDir != "/home/ds/gascity-worktrees/polecat-1" {
		t.Errorf("session attribution = %q/%q/%q, want the values stamped on the root", p.SessionName, p.SessionID, p.WorkDir)
	}
	if p.Ts.IsZero() {
		t.Errorf("Ts is zero, want a resolution timestamp")
	}

	// A second run finds nothing, so it must publish nothing more.
	if _, err := reapOrphanMolecules(store, rec, reapOrphansOptions{}, &out); err != nil {
		t.Fatalf("second reapOrphanMolecules: %v", err)
	}
	if n := len(rec.Events); n != 2 {
		t.Errorf("event count after idempotent re-run = %d, want 2", n)
	}
}

// TestReapOrphans_DryRunPublishesNoEvents pins that a preview closes nothing
// and therefore announces nothing.
func TestReapOrphans_DryRunPublishesNoEvents(t *testing.T) {
	store := beads.NewMemStore()
	orphanShape(t, store, 2)

	rec := events.NewFake()
	var out bytes.Buffer
	if _, err := reapOrphanMolecules(store, rec, reapOrphansOptions{DryRun: true}, &out); err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("dry-run published %d events, want 0: %+v", len(rec.Events), rec.Events)
	}
}

// TestReapOrphans_NonOrphanPublishesNoEvents pins that a molecule left alone
// (an open target keeps it parked) is not announced as resolved.
func TestReapOrphans_NonOrphanPublishesNoEvents(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "mol-tdd-build", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "live target",
		Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var out bytes.Buffer
	if _, err := reapOrphanMolecules(store, rec, reapOrphansOptions{}, &out); err != nil {
		t.Fatalf("reapOrphanMolecules: %v", err)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("non-orphan published %d events, want 0: %+v", len(rec.Events), rec.Events)
	}
}
