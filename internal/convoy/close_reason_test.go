package convoy

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// reasonCloserStore is BdStore-shaped: its close path takes the reason
// directly. It records the reasons it was handed.
type reasonCloserStore struct {
	*beads.MemStore
	reasons []string
}

func (s *reasonCloserStore) CloseWithReason(id, reason string) error {
	s.reasons = append(s.reasons, reason)
	return s.Close(id)
}

// TestCloseWithReasonReachesTheExplicitReasonCloserThroughAHandle pins that
// CloseWithReason hands the reason to a store whose close path takes one,
// reached through the work handle (typed-stores T5a), instead of falling back
// to a plain Close.
func TestCloseWithReasonReachesTheExplicitReasonCloserThroughAHandle(t *testing.T) {
	store := &reasonCloserStore{MemStore: beads.NewMemStore()}
	c, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	if err := CloseWithReason(beadstest.Work(store), c.ID, "all work landed"); err != nil {
		t.Fatalf("CloseWithReason: %v", err)
	}
	if len(store.reasons) != 1 || store.reasons[0] != "all work landed" {
		t.Errorf("the explicit-reason close got %q, want one call with the reason", store.reasons)
	}
	got, err := store.Get(c.ID)
	if err != nil || got.Status != "closed" || got.Metadata["close_reason"] != "all work landed" {
		t.Errorf("convoy = %+v (%v), want closed with close_reason stamped", got, err)
	}
}
