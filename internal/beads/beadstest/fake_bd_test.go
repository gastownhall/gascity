package beadstest

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestFakeBdUpdateUnsetsMetadataThroughBdStore pins the fake's --unset-metadata:
// a BdStore unset round-trips through it and the key is gone, so a BdStore
// case cannot pass with the key still set.
func TestFakeBdUpdateUnsetsMetadataThroughBdStore(t *testing.T) {
	store := NewFakeBd().NewBdStore()
	created, err := store.Create(beads.Bead{Title: "unset target", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Update(created.ID, beads.UpdateOpts{Metadata: map[string]string{"kept": "1", "gone": "2"}}); err != nil {
		t.Fatalf("Update(set): %v", err)
	}
	if err := store.Update(created.ID, beads.UpdateOpts{UnsetMetadata: []string{"gone"}}); err != nil {
		t.Fatalf("Update(unset): %v", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got.Metadata["gone"]; ok || got.Metadata["kept"] != "1" {
		t.Errorf("metadata = %v, want only kept=1", got.Metadata)
	}
}

// TestFakeBdUpdateRefusesAFlagItDoesNotModel pins the other half: an update
// flag the fake does not apply fails the command rather than being dropped.
func TestFakeBdUpdateRefusesAFlagItDoesNotModel(t *testing.T) {
	f := NewFakeBd()
	created, err := f.NewBdStore().Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = f.Run("", "bd", "update", "--json", created.ID, "--add-label", "x")
	if err == nil || !strings.Contains(err.Error(), "--add-label") {
		t.Fatalf("update --add-label = %v, want a refusal naming the flag", err)
	}
	got, err := f.NewBdStore().Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Labels) != 0 {
		t.Errorf("labels = %v, want the refused update to write nothing", got.Labels)
	}
}

// TestFakeBdUpdateEnforcesItsPreconditions pins the fake's --if-status and
// --if-assignee: a BdStore compare-and-swap that does not hold writes nothing
// and reports the lost swap, and one that holds writes.
func TestFakeBdUpdateEnforcesItsPreconditions(t *testing.T) {
	store := NewFakeBd().NewBdStore()
	created, err := store.Create(beads.Bead{Title: "fenced", Type: "task", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed := "closed"
	ok, err := store.UpdateIfAssignment(created.ID, "open", "worker-2", beads.UpdateOpts{Status: &closed})
	if err != nil || ok {
		t.Fatalf("UpdateIfAssignment(wrong assignee) = (%t, %v), want a lost swap", ok, err)
	}
	if got, err := store.Get(created.ID); err != nil || got.Status != "open" {
		t.Fatalf("after a lost swap: status = %q (%v), want open", got.Status, err)
	}
	ok, err = store.UpdateIfAssignment(created.ID, "open", "worker-1", beads.UpdateOpts{Status: &closed})
	if err != nil || !ok {
		t.Fatalf("UpdateIfAssignment(holder) = (%t, %v), want the swap", ok, err)
	}
	if got, err := store.Get(created.ID); err != nil || got.Status != "closed" {
		t.Fatalf("after the swap: status = %q (%v), want closed", got.Status, err)
	}
}
