package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// seedBlockedThenWorkableInProgress creates, in canonical ready order, an
// in_progress bead with an open blocker and then a workable in_progress bead,
// both assigned to worker-1.
func seedBlockedThenWorkableInProgress(t *testing.T) (beads.Store, beads.Bead, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	gate := mustCreateDrainAckBead(t, store, beads.Bead{Title: "the gate", Type: "task"}, "", "")
	blocked := mustCreateDrainAckBead(t, store, beads.Bead{Title: "blocked step", Type: "task"}, "in_progress", "worker-1")
	workable := mustCreateDrainAckBead(t, store, beads.Bead{Title: "workable step", Type: "task"}, "in_progress", "worker-1")
	if err := store.DepAdd(blocked.ID, gate.ID, "blocks"); err != nil {
		t.Fatalf("adding the blocking dep: %v", err)
	}
	return store, blocked, workable
}

// TestGcReadyInProgressReadyDropsBlockedRowsBeforeLimit is the federated arm of
// [beads] bd_list_ready_status: with --ready, a blocked first row no longer
// fills the crash-recovery tier's --limit=1 read.
func TestGcReadyInProgressReadyDropsBlockedRowsBeforeLimit(t *testing.T) {
	store, blocked, workable := seedBlockedThenWorkableInProgress(t)
	legs := []readyLeg{readyTestLeg("city", store)}

	rows, err := readyBeadsForOpts(legs, readyOpts{status: readyStatusInProgress, assignee: "worker-1", limit: 1})
	if err != nil {
		t.Fatalf("readyBeadsForOpts: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != blocked.ID || len(rows[0].BlockedBy) != 1 {
		t.Fatalf("without --ready: rows = %+v, want the blocked row %s carrying its blocker", rows, blocked.ID)
	}

	rows, err = readyBeadsForOpts(legs, readyOpts{status: readyStatusInProgress, assignee: "worker-1", limit: 1, ready: true})
	if err != nil {
		t.Fatalf("readyBeadsForOpts(--ready): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != workable.ID {
		t.Fatalf("with --ready: rows = %+v, want the workable row %s", rows, workable.ID)
	}
}

// TestGcReadyInProgressReadyKeepsRowsWithClosedBlockers is the control: a
// closed blocker does not block, and the row still carries blocked_by.
func TestGcReadyInProgressReadyKeepsRowsWithClosedBlockers(t *testing.T) {
	store, blocked, _ := seedBlockedThenWorkableInProgress(t)
	deps, err := store.DepList(blocked.ID, "down")
	if err != nil || len(deps) != 1 {
		t.Fatalf("DepList = %+v, %v", deps, err)
	}
	if err := store.Close(deps[0].DependsOnID); err != nil {
		t.Fatalf("closing the blocker: %v", err)
	}

	rows, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", store)}, readyOpts{status: readyStatusInProgress, assignee: "worker-1", limit: 1, ready: true})
	if err != nil {
		t.Fatalf("readyBeadsForOpts(--ready): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != blocked.ID || len(rows[0].BlockedBy) != 1 || rows[0].BlockedBy[0].Status != "closed" {
		t.Fatalf("rows = %+v, want %s with its closed blocker", rows, blocked.ID)
	}
}

// TestGcReadyInProgressReadyOnlyBlockedRowsIsEmpty: nothing workable, nothing
// served.
func TestGcReadyInProgressReadyOnlyBlockedRowsIsEmpty(t *testing.T) {
	store, _, workable := seedBlockedThenWorkableInProgress(t)
	if err := store.Close(workable.ID); err != nil {
		t.Fatalf("closing the workable row: %v", err)
	}
	rows, err := readyBeadsForOpts([]readyLeg{readyTestLeg("city", store)}, readyOpts{status: readyStatusInProgress, assignee: "worker-1", limit: 1, ready: true})
	if err != nil {
		t.Fatalf("readyBeadsForOpts(--ready): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}

// TestGcReadyReadyFlagIsRegistered pins the flag the generated federated
// crash-recovery read passes.
func TestGcReadyReadyFlagIsRegistered(t *testing.T) {
	cmd := newReadyCmd(nil, nil)
	if err := cmd.ParseFlags([]string{"--status", "in_progress", "--ready", "--exclude-label=hold:mayor", "--limit=1"}); err != nil {
		t.Fatalf("parsing the crash-recovery flags: %v", err)
	}
}
