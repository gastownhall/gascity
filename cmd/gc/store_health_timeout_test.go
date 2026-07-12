package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeHealthStore is a beads.Store whose List and Count behavior the test
// controls. The embedded nil interface is never used — liveRowCount only calls
// Count (via the beads.Counter assertion) and List.
type fakeHealthStore struct {
	beads.Store
	countFn func(context.Context, beads.ListQuery) (int, error)
	listFn  func(beads.ListQuery) ([]beads.Bead, error)
}

func (f *fakeHealthStore) Count(ctx context.Context, q beads.ListQuery, _ ...string) (int, error) {
	return f.countFn(ctx, q)
}

func (f *fakeHealthStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	return f.listFn(q)
}

// TestLiveRowCountBoundsSlowScan is the regression for the ~105s silent stall in
// `gc status`: liveRowCount ran an unbounded IncludeClosed full-history scan
// (store.List) with no timeout, so a live city with a large closed-history
// table hung status for ~2 minutes. When the Counter cannot answer, the scan
// must be bounded and return 0 (best-effort) rather than stall.
func TestLiveRowCountBoundsSlowScan(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // let the leaked List goroutine exit
	store := &fakeHealthStore{
		countFn: func(context.Context, beads.ListQuery) (int, error) {
			return 0, errors.New("count unsupported for this query")
		},
		listFn: func(beads.ListQuery) ([]beads.Bead, error) {
			<-release // simulate the multi-minute closed-history hydration
			return nil, nil
		},
	}

	start := time.Now()
	got := liveRowCount(store)
	elapsed := time.Since(start)

	if got != 0 {
		t.Fatalf("liveRowCount = %d, want 0 when the scan times out", got)
	}
	if elapsed > statusStoreHealthTimeout+2*time.Second {
		t.Fatalf("liveRowCount did not bound the scan: took %s (bound %s)", elapsed, statusStoreHealthTimeout)
	}
}

// TestLiveRowCountUsesCounterFastPath pins that a Counter-capable store answers
// from the catalog without hydrating rows — List must not be called.
func TestLiveRowCountUsesCounterFastPath(t *testing.T) {
	store := &fakeHealthStore{
		countFn: func(_ context.Context, q beads.ListQuery) (int, error) {
			if !q.IncludeClosed {
				t.Errorf("row-footprint count must IncludeClosed, got query %+v", q)
			}
			return 42, nil
		},
		listFn: func(beads.ListQuery) ([]beads.Bead, error) {
			t.Fatal("List must not be called when the Counter answers")
			return nil, nil
		},
	}

	if got := liveRowCount(store); got != 42 {
		t.Fatalf("liveRowCount = %d, want 42 from the Counter fast path", got)
	}
}
