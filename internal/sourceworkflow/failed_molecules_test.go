package sourceworkflow

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// strictCloseOrderStore rejects a CloseAll batch that closes a parent before
// an open child or a blocked bead before its open blocker, as a strict
// backing store does.
type strictCloseOrderStore struct {
	*beads.MemStore
}

func (s *strictCloseOrderStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closing := make(map[string]bool, len(ids))
	for _, id := range ids {
		kids, err := s.List(beads.ListQuery{ParentID: id, TierMode: beads.TierBoth})
		if err != nil {
			return 0, err
		}
		for _, kid := range kids {
			if kid.Status != "closed" && !closing[kid.ID] {
				return 0, fmt.Errorf("strict store: closing parent %s before open child %s", id, kid.ID)
			}
		}
		deps, err := s.DepList(id, "down")
		if err != nil {
			return 0, err
		}
		for _, d := range deps {
			if d.Type != "blocks" {
				continue
			}
			blocker, err := s.Get(d.DependsOnID)
			if err != nil {
				return 0, err
			}
			if blocker.Status != "closed" && !closing[blocker.ID] {
				return 0, fmt.Errorf("strict store: closing blocked %s before open blocker %s", id, blocker.ID)
			}
		}
		closing[id] = true
	}
	return s.MemStore.CloseAll(ids, metadata)
}

// failedMeta is metadata carrying the molecule_failed stamp.
func failedMeta() map[string]string {
	return map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"}
}

func mustCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%q): %v", b.Title, err)
	}
	return created
}

func mustGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// assertSwept fails unless every id is closed by the sweep and still stamped.
func assertSwept(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		b := mustGet(t, store, id)
		if b.Status != "closed" {
			t.Errorf("bead %s (%q) status = %q, want closed", id, b.Title, b.Status)
			continue
		}
		if got := b.Metadata["close_reason"]; got != FailedMoleculeSweepCloseReason {
			t.Errorf("bead %s close_reason = %q, want the sweep's", id, got)
		}
		if got := b.Metadata[beadmeta.OutcomeMetadataKey]; got != beadmeta.OutcomeSkipped {
			t.Errorf("bead %s gc.outcome = %q, want skipped", id, got)
		}
		if b.Metadata[beadmeta.MoleculeFailedMetadataKey] != "true" {
			t.Errorf("bead %s lost molecule_failed on close", id)
		}
	}
}

// pastGrace is a cutoff every bead created so far is at or before.
func pastGrace() time.Time { return time.Now().Add(time.Hour) }

// AC-2: a failed v1 molecule has no gc.kind=workflow root and its steps link
// to it by ParentID alone; the sweep closes all of them in one ordered batch a
// strict store accepts.
func TestSweepFailedMoleculesClosesV1ParentLinkedChain(t *testing.T) {
	store := &strictCloseOrderStore{MemStore: beads.NewMemStore()}
	root := mustCreate(t, store, beads.Bead{Title: "v1 molecule", Type: "molecule", Metadata: failedMeta()})
	step1 := mustCreate(t, store, beads.Bead{Title: "step 1", ParentID: root.ID, Metadata: failedMeta()})
	step2 := mustCreate(t, store, beads.Bead{Title: "step 2", ParentID: root.ID, Metadata: failedMeta()})
	nested := mustCreate(t, store, beads.Bead{Title: "step 2.1", ParentID: step2.ID, Metadata: failedMeta()})
	if err := store.DepAdd(step2.ID, step1.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	got, err := SweepFailedMolecules(beadstest.WorkMolecule(store), pastGrace(), 0)
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(got.Chains) != 1 || got.Chains[0] != root.ID || got.Closed != 4 {
		t.Fatalf("sweep = %+v, want one chain %s with 4 beads closed", got, root.ID)
	}
	assertSwept(t, store, root.ID, step1.ID, step2.ID, nested.ID)
}

// assertOpen fails unless every id is still open.
func assertOpen(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if b := mustGet(t, store, id); b.Status != "open" {
			t.Errorf("bead %s (%q) status = %q, want open", id, b.Title, b.Status)
		}
	}
}

// AC-3: a failure stamped exactly at cutoff is past grace and one a
// nanosecond later is not; a zero UpdatedAt falls back to CreatedAt. A bead
// that did not fail, a controller-owned row and a chain whose root is
// stamped gc.gc_exempt=true are never touched.
func TestSweepFailedMoleculesHonorsGraceAndExemptions(t *testing.T) {
	cutoff := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	long := cutoff.Add(-time.Hour)
	failed := map[string]string{beadmeta.MoleculeFailedMetadataKey: "true"}
	exempt := map[string]string{beadmeta.MoleculeFailedMetadataKey: "true", beadmeta.GCExemptMetadataKey: "true"}
	seed := []beads.Bead{
		{ID: "at-cutoff", Title: "stamped at cutoff", Status: "open", Metadata: failed, CreatedAt: long, UpdatedAt: cutoff},
		{ID: "inside", Title: "stamped 1ns after cutoff", Status: "open", Metadata: failed, CreatedAt: long, UpdatedAt: cutoff.Add(time.Nanosecond)},
		{ID: "created-past", Title: "no update stamp, created before cutoff", Status: "open", Metadata: failed, CreatedAt: cutoff.Add(-time.Minute).Add(time.Second)},
		{ID: "created-inside", Title: "no update stamp, created after cutoff", Status: "open", Metadata: failed, CreatedAt: cutoff.Add(time.Minute)},
		{ID: "not-failed", Title: "open bead that did not fail", Status: "open", CreatedAt: long.Add(time.Second), UpdatedAt: long},
		{ID: "session", Title: "controller-owned row", Type: "session", Status: "open", Metadata: failed, CreatedAt: long.Add(2 * time.Second), UpdatedAt: long},
		{ID: "exempt", Title: "exempt failed root", Status: "open", Metadata: exempt, CreatedAt: long.Add(3 * time.Second), UpdatedAt: long},
		{ID: "exempt-step", Title: "step of the exempt root", ParentID: "exempt", Status: "open", Metadata: failed, CreatedAt: long.Add(4 * time.Second), UpdatedAt: long},
	}
	store := beads.NewMemStoreFrom(0, seed, nil)

	got, err := SweepFailedMolecules(beadstest.WorkMolecule(store), cutoff, 0)
	if err != nil {
		t.Fatalf("SweepFailedMolecules: %v", err)
	}
	if len(got.Chains) != 2 || got.Chains[0] != "at-cutoff" || got.Chains[1] != "created-past" || got.Closed != 2 {
		t.Fatalf("sweep = %+v, want chains [at-cutoff created-past], oldest first, with 2 beads closed", got)
	}
	assertSwept(t, store, "at-cutoff", "created-past")
	assertOpen(t, store, "inside", "created-inside", "not-failed", "session", "exempt", "exempt-step")
}
