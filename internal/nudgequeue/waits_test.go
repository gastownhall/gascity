package nudgequeue

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type listQueryCaptureStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *listQueryCaptureStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return s.Store.List(query)
}

func TestMarkTerminalUsesBoundedNudgeLookup(t *testing.T) {
	mem := beads.NewMemStore()
	nudge, err := mem.Create(beads.Bead{
		Title:  "nudge",
		Labels: []string{"nudge:nudge-123"},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	store := &listQueryCaptureStore{Store: mem}

	if err := markTerminal(store, "nudge-123", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("markTerminal: %v", err)
	}

	if len(store.queries) != 1 {
		t.Fatalf("List calls = %d, want 1", len(store.queries))
	}
	if got := store.queries[0].Limit; got != nudgeLookupLimit {
		t.Fatalf("List limit = %d, want %d", got, nudgeLookupLimit)
	}
	updated, err := mem.Get(nudge.ID)
	if err != nil {
		t.Fatalf("Get(nudge): %v", err)
	}
	if updated.Status != "closed" {
		t.Fatalf("nudge status = %q, want closed", updated.Status)
	}
}
