package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestSessionIndex_Populate(t *testing.T) {
	store := beads.NewMemStore()

	// Create two open session beads.
	b1, _ := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"template":     "worker",
			"state":        "active",
			"generation":   "1",
		},
	})
	_, _ = store.Create(beads.Bead{
		Title:  "worker-2",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-2",
			"template":     "worker",
			"state":        "asleep",
			"generation":   "1",
		},
	})

	// Create a closed bead — should be excluded.
	closedB, _ := store.Create(beads.Bead{
		Title:  "worker-old",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-old",
			"template":     "worker",
			"state":        "active",
		},
	})
	_ = store.Close(closedB.ID)

	idx := newSessionIndex()
	var stderr bytes.Buffer
	idx.populateIndex(store, &stderr)

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	snap := idx.snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	if snap[b1.ID] == nil {
		t.Error("missing worker-1 entry")
	}
}

func TestSessionIndex_Occupancy(t *testing.T) {
	idx := newSessionIndex()
	idx.update("b1", &sessionEntry{template: "worker", state: "active"})
	idx.update("b2", &sessionEntry{template: "worker", state: "creating"})
	idx.update("b3", &sessionEntry{template: "worker", state: "quarantined"})
	idx.update("b4", &sessionEntry{template: "worker", state: "archived"})                       // does NOT count
	idx.update("b5", &sessionEntry{template: "worker", state: "asleep", sleepReason: "drained"}) // does NOT count
	idx.update("b6", &sessionEntry{template: "worker", state: "drained"})                        // legacy drained also does NOT count
	idx.update("b7", &sessionEntry{template: "other", state: "active"})

	if occ := idx.occupancy("worker"); occ != 3 {
		t.Errorf("worker occupancy = %d, want 3", occ)
	}
	if occ := idx.occupancy("other"); occ != 1 {
		t.Errorf("other occupancy = %d, want 1", occ)
	}
	if occ := idx.occupancy("missing"); occ != 0 {
		t.Errorf("missing occupancy = %d, want 0", occ)
	}
}

func TestSessionIndex_Update(t *testing.T) {
	idx := newSessionIndex()
	idx.update("b1", &sessionEntry{template: "worker", state: "active"})

	if e := idx.get("b1"); e == nil || e.state != "active" {
		t.Fatal("expected active entry")
	}

	// Update state.
	idx.update("b1", &sessionEntry{template: "worker", state: "draining"})
	if e := idx.get("b1"); e == nil || e.state != "draining" {
		t.Error("expected draining state after update")
	}
}

func TestSessionIndex_Remove(t *testing.T) {
	idx := newSessionIndex()
	idx.update("b1", &sessionEntry{template: "worker", state: "active"})
	idx.remove("b1")

	if e := idx.get("b1"); e != nil {
		t.Error("expected nil after remove")
	}
}

func TestSessionIndex_ByTemplate(t *testing.T) {
	idx := newSessionIndex()
	idx.update("b1", &sessionEntry{template: "worker", state: "active"})
	idx.update("b2", &sessionEntry{template: "worker", state: "asleep"})
	idx.update("b3", &sessionEntry{template: "mayor", state: "active"})

	workers := idx.byTemplate("worker")
	if len(workers) != 2 {
		t.Errorf("expected 2 workers, got %d", len(workers))
	}

	mayors := idx.byTemplate("mayor")
	if len(mayors) != 1 {
		t.Errorf("expected 1 mayor, got %d", len(mayors))
	}
}

func TestSessionIndex_NilStore(t *testing.T) {
	idx := newSessionIndex()
	var stderr bytes.Buffer
	idx.populateIndex(nil, &stderr)
	// Should not panic, index should be empty.
	if len(idx.snapshot()) != 0 {
		t.Error("expected empty index for nil store")
	}
}
