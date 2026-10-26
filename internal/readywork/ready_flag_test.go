package readywork

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// seedBlockedThenWorkableInProgress creates, in canonical ready order, an
// in_progress bead with an open blocker and then a workable in_progress bead,
// both assigned to worker-1.
func seedBlockedThenWorkableInProgress(t *testing.T) (*beads.MemStore, beads.Bead, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	first, second := 1, 2
	gate := mustCreate(t, store, beads.Bead{Title: "the gate", Type: "task"})
	blocked := mustCreate(t, store, beads.Bead{Title: "blocked step", Type: "task", Priority: &first})
	workable := mustCreate(t, store, beads.Bead{Title: "workable step", Type: "task", Priority: &second})
	claimInProgress(t, store, blocked.ID, workable.ID)
	if err := store.DepAdd(blocked.ID, gate.ID, "blocks"); err != nil {
		t.Fatalf("adding the blocking dep: %v", err)
	}
	return store, gate, blocked, workable
}

func claimInProgress(t *testing.T, store beads.Store, ids ...string) {
	t.Helper()
	status, assignee := StatusInProgress, "worker-1"
	for _, id := range ids {
		if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("claim %s: %v", id, err)
		}
	}
}

// TestReadyDropsBlockedInProgressRowsBeforeLimit is the crash-recovery read
// with --ready: a blocked first row no longer fills --limit=1 and hides the
// workable row behind it.
func TestReadyDropsBlockedInProgressRowsBeforeLimit(t *testing.T) {
	store, _, blocked, workable := seedBlockedThenWorkableInProgress(t)
	legs := []Leg{testLeg("city", store)}

	rows, err := Read(legs, Query{Status: StatusInProgress, Assignee: "worker-1", Limit: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != blocked.ID || len(rows[0].BlockedBy) != 1 {
		t.Fatalf("without Ready: rows = %+v, want the blocked row %s carrying its blocker", rows, blocked.ID)
	}

	rows, err = Read(legs, Query{Status: StatusInProgress, Assignee: "worker-1", Limit: 1, Ready: true})
	if err != nil {
		t.Fatalf("Read(Ready): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != workable.ID {
		t.Fatalf("with Ready: rows = %+v, want the workable row %s", rows, workable.ID)
	}
}

// TestReadyKeepsInProgressRowsWithClosedBlockers: a closed blocker does not
// block, and the row still carries blocked_by.
func TestReadyKeepsInProgressRowsWithClosedBlockers(t *testing.T) {
	store, gate, blocked, _ := seedBlockedThenWorkableInProgress(t)
	if err := store.Close(gate.ID); err != nil {
		t.Fatalf("closing the blocker: %v", err)
	}
	rows, err := Read([]Leg{testLeg("city", store)}, Query{Status: StatusInProgress, Assignee: "worker-1", Limit: 1, Ready: true})
	if err != nil {
		t.Fatalf("Read(Ready): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != blocked.ID || len(rows[0].BlockedBy) != 1 || rows[0].BlockedBy[0].Status != "closed" {
		t.Fatalf("rows = %+v, want %s with its closed blocker", rows, blocked.ID)
	}
}

// TestReadyWithOnlyBlockedInProgressRowsIsEmpty: nothing workable, nothing
// served.
func TestReadyWithOnlyBlockedInProgressRowsIsEmpty(t *testing.T) {
	store, _, _, workable := seedBlockedThenWorkableInProgress(t)
	if err := store.Close(workable.ID); err != nil {
		t.Fatalf("closing the workable row: %v", err)
	}
	rows, err := Read([]Leg{testLeg("city", store)}, Query{Status: StatusInProgress, Assignee: "worker-1", Limit: 1, Ready: true})
	if err != nil {
		t.Fatalf("Read(Ready): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}

// TestReadyFiltersDirectBlockersOnly pins the scope of the filter: it drops a
// row with a non-closed blocking dependency of its own, and keeps a deferred
// row and a row whose parent is blocked. bd's --ready drops those too; this
// reader does not.
func TestReadyFiltersDirectBlockersOnly(t *testing.T) {
	store := beads.NewMemStore()
	gate := mustCreate(t, store, beads.Bead{Title: "the gate", Type: "task"})
	epic := mustCreate(t, store, beads.Bead{Title: "blocked epic", Type: "epic"})
	if err := store.DepAdd(epic.ID, gate.ID, "blocks"); err != nil {
		t.Fatalf("blocking the epic: %v", err)
	}
	later := time.Now().Add(24 * time.Hour)
	deferred := mustCreate(t, store, beads.Bead{Title: "deferred", Type: "task", DeferUntil: &later})
	child := mustCreate(t, store, beads.Bead{Title: "child of blocked epic", Type: "task", ParentID: epic.ID})
	claimInProgress(t, store, deferred.ID, child.ID)

	rows, err := Read([]Leg{testLeg("city", store)}, Query{Status: StatusInProgress, Assignee: "worker-1", Ready: true})
	if err != nil {
		t.Fatalf("Read(Ready): %v", err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if len(rows) != 2 || !got[deferred.ID] || !got[child.ID] {
		t.Fatalf("rows = %+v, want the deferred row %s and the parent-blocked row %s", rows, deferred.ID, child.ID)
	}
}

// TestReadyLeavesTheReadyArmUnchanged: the ready arm is ready already, so the
// flag changes nothing there.
func TestReadyLeavesTheReadyArmUnchanged(t *testing.T) {
	store := beads.NewMemStore()
	for _, title := range []string{"a", "b", "c"} {
		mustCreate(t, store, beads.Bead{Title: title, Type: "task"})
	}
	legs := []Leg{testLeg("city", store)}
	plain, err := Read(legs, Query{Limit: 2})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	withReady, err := Read(legs, Query{Limit: 2, Ready: true})
	if err != nil {
		t.Fatalf("Read(Ready): %v", err)
	}
	if len(plain) != 2 || len(withReady) != 2 || plain[0].ID != withReady[0].ID || plain[1].ID != withReady[1].ID {
		t.Fatalf("ready arm: plain %+v, with Ready %+v; want the same rows", plain, withReady)
	}
}
