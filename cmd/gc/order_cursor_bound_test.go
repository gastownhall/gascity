package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type captureListStore struct {
	beads.Store
	got beads.ListQuery
}

func (s *captureListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.got = q
	return s.Store.List(q)
}

// bdCursor computes MAX(seq:<n>) across an order's tracking beads. The seq
// cursor is forward-only — every new run records a seq >= all prior runs —
// so the max always lives among the newest runs and the read must be bounded
// (sr-dp9o: the unbounded IncludeClosed list scaled with the whole retained
// tracking corpus, ~0.3s per event order per tick).
func TestBdCursorBoundsItsTrackingRead(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "run", Labels: []string{"order:digest", "seq:42"}}); err != nil {
		t.Fatal(err)
	}
	store := &captureListStore{Store: mem}

	got, err := bdCursor(store, "digest")
	if err != nil {
		t.Fatalf("bdCursor: %v", err)
	}
	if got != 42 {
		t.Fatalf("bdCursor() = %d, want 42", got)
	}
	if store.got.Limit <= 0 {
		t.Fatalf("bdCursor list Limit = %d, want a bounded newest-first read", store.got.Limit)
	}
	if store.got.Sort != beads.SortCreatedDesc {
		t.Fatalf("bdCursor list Sort = %q, want SortCreatedDesc", store.got.Sort)
	}
	// bdCursor reduces the rows to MaxSeqFromLabels — a max over seq, NOT over the
	// created_at sort key — so it must NOT opt into the backing's bounded
	// created-desc read. The backing breaks created_at ties by id ASC, so a bounded
	// backing read would drop the newest largest-id row that carries the max seq
	// when a same-second burst exceeds the cap, regressing the event cursor into
	// replaying consumed events. The read stays exact by fetching the full candidate
	// set and letting ApplyListQuery cut the canonical (created_at DESC, id DESC)
	// prefix; the Limit is only a client-side result cap.
	if store.got.AllowBackingCreatedLimit {
		t.Fatal("bdCursor must NOT set AllowBackingCreatedLimit: its max(seq) reduction needs the canonical id-DESC prefix, which a bounded id-ASC backing read would truncate")
	}
}
