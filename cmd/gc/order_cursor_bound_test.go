package main

import (
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
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
	if _, err := mem.Create(beads.Bead{Title: "run", Labels: []string{"order-run:digest", "order:digest", "seq:42", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	store := &captureListStore{Store: mem}

	got, err := bdCursor(store, "digest")
	if err != nil {
		t.Fatalf("bdCursor: %v", err)
	}
	if !slices.Equal(got, []uint64{42}) {
		t.Fatalf("bdCursor() = %v, want [42]", got)
	}
	if store.got.Limit <= 0 {
		t.Fatalf("bdCursor list Limit = %d, want a bounded newest-first read", store.got.Limit)
	}
	if store.got.Sort != beads.SortCreatedDesc {
		t.Fatalf("bdCursor list Sort = %q, want SortCreatedDesc", store.got.Sort)
	}
	// The event check reduces bdCursor's stamps to a max over seq, NOT over the
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

// TestBdCursorIgnoresAForgedSeq (the #7766 review): a plain bead carrying an
// event order's order:<name> and seq:<n> labels, which the generic API still
// creates, moves no dispatcher cursor; the run record's seq is the cursor.
func TestBdCursorIgnoresAForgedSeq(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "run", Labels: []string{"order-run:digest", "order:digest", "seq:5", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Create(beads.Bead{Title: "forged", Type: "task", Labels: []string{"order-run:digest", "order:digest", "seq:999999"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := bdCursor(mem, "digest"); err != nil || !slices.Equal(got, []uint64{5}) {
		t.Fatalf("bdCursor = %v (%v), want the run record's [5]", got, err)
	}
}

// TestBdCursorSurvivesAnOrderLabelFlood (trial, the #7766 review): more plain
// beads carrying order:<name> than the window holds, all newer than the run,
// neither reset nor move the dispatcher's cursor.
func TestBdCursorSurvivesAnOrderLabelFlood(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "run", Labels: []string{"order-run:digest", "order:digest", "seq:5", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	for range bdCursorRecentRunsLimit + 1 {
		if _, err := mem.Create(beads.Bead{Title: "flood", Type: "task", Labels: []string{"order:digest", "seq:999999"}}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := bdCursor(mem, "digest"); err != nil || !slices.Equal(got, []uint64{5}) {
		t.Fatalf("bdCursor = %v (%v), want the run record's [5]", got, err)
	}
}

// TestEventOrderFiresAfterEventLogRestartsBelowStoredCursor (#8567): when the
// event log restarts at seq 1 while the bead store keeps a run record stamped
// with an old, higher seq, the order must still fire on the new log's
// matching events instead of staying silent until the log passes the old seq.
func TestEventOrderFiresAfterEventLogRestartsBelowStoredCursor(t *testing.T) {
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "run", Labels: []string{"order-run:digest", "order:digest", "seq:500", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	ep := events.NewFake()
	ep.Record(events.Event{Type: "bead.updated"})
	ep.Record(events.Event{Type: "bead.updated"})

	a := orders.Order{Name: "digest", Trigger: "event", On: "bead.updated"}
	stamps, err := bdCursorAcrossStores(a.ScopedName(), mem)
	if err != nil {
		t.Fatalf("bdCursorAcrossStores: %v", err)
	}
	neverRan := func(string) (time.Time, error) { return time.Time{}, nil }
	check := orders.CheckTrigger(a, time.Now(), neverRan, ep, func(string) []uint64 { return stamps })
	if !check.Due {
		head, _ := ep.LatestSeq()
		t.Fatalf("order not due after event log restart: stamps=%v log head=%d reason=%q; want due on the 2 new bead.updated events", stamps, head, check.Reason)
	}
}
