package session

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type rejectLegacyWaitTypeQueryStore struct {
	*beads.MemStore
}

func (s rejectLegacyWaitTypeQueryStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == LegacyWaitBeadType {
		return nil, errors.New("legacy wait type query should not be used")
	}
	return s.MemStore.List(query)
}

func TestWaitNudgeIDs_AcceptsLegacyWaitBeadsWithoutLegacyTypeQuery(t *testing.T) {
	store := rejectLegacyWaitTypeQueryStore{MemStore: beads.NewMemStore()}
	if _, err := store.Create(beads.Bead{
		Type:   LegacyWaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
			"nudge_id":   "wait-nudge",
		},
	}); err != nil {
		t.Fatalf("create legacy wait: %v", err)
	}

	got, err := WaitNudgeIDs(store, "gc-session")
	if err != nil {
		t.Fatalf("WaitNudgeIDs: %v", err)
	}
	if len(got) != 1 || got[0] != "wait-nudge" {
		t.Fatalf("WaitNudgeIDs = %#v, want [wait-nudge]", got)
	}
}

func TestCancelWaits_CancelsLegacyWaitBeadsWithoutLegacyTypeQuery(t *testing.T) {
	store := rejectLegacyWaitTypeQueryStore{MemStore: beads.NewMemStore()}
	wait, err := store.Create(beads.Bead{
		Type:   LegacyWaitBeadType,
		Labels: []string{WaitBeadLabel, "session:gc-session"},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create legacy wait: %v", err)
	}

	if err := CancelWaits(store, "gc-session", time.Now().UTC()); err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	updated, err := store.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updated.Metadata["state"] != waitStateCanceled {
		t.Fatalf("state = %q, want %q", updated.Metadata["state"], waitStateCanceled)
	}
	if updated.Status != "closed" {
		t.Fatalf("status = %q, want closed", updated.Status)
	}
}
