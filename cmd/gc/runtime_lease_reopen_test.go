package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestReopenNamedSessionBatchClearsTheRuntimeLease: a reopened row holds no
// runtime lease, so a close that left the record cannot hand it back to its
// old holder (SESSION-RUNTIME-012).
func TestReopenNamedSessionBatchClearsTheRuntimeLease(t *testing.T) {
	for _, state := range []string{"active", "stopped"} {
		batch := reopenNamedSessionBatch(state, "", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
		for k, v := range session.RuntimeLeaseClearPatch() {
			if got, ok := batch[k]; !ok || got != v {
				t.Errorf("reopen to %s: %s = %q (set %v), want cleared", state, k, got, ok)
			}
		}
		if _, ok := batch[session.RuntimeLeaseEpochKey]; ok {
			t.Errorf("reopen to %s writes the epoch, which only grows", state)
		}
	}
}

// TestBdStoreBridgeReopenClearsTheRuntimeLease: the bd bridge's reopen of a
// closed session bead clears its runtime lease record, keeping the epoch.
func TestBdStoreBridgeReopenClearsTheRuntimeLease(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title: "worker", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "worker", session.RuntimeLeaseHolderKey: "h/1/n", session.RuntimeLeaseEpochKey: "3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := bdStoreBridgeReopen(store, b.ID); err != nil {
		t.Fatal(err)
	}
	got := mustGetBead(t, store, b.ID)
	if got.Status != "open" || got.Metadata[session.RuntimeLeaseHolderKey] != "" || got.Metadata[session.RuntimeLeaseEpochKey] != "3" {
		t.Fatalf("reopened session = %s %v, want open with no lease holder and the epoch kept", got.Status, got.Metadata)
	}
}
