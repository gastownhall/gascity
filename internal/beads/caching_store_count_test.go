package beads_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// countingBackingStore is a Store + Counter fake that records Count calls.
type countingBackingStore struct {
	beads.Store
	countCalls  int
	countResult int
	countErr    error
	gotQuery    beads.ListQuery
	gotExcludes []string
}

func (s *countingBackingStore) Count(_ context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	s.countCalls++
	s.gotQuery = query
	s.gotExcludes = excludeTypes
	return s.countResult, s.countErr
}

func TestCachingStoreCountServedFromCache(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	mustCreate := func(b beads.Bead, status string) {
		t.Helper()
		created, err := mem.Create(b)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if status != "open" {
			if err := mem.Update(created.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatalf("Update(%s): %v", status, err)
			}
		}
	}
	mustCreate(beads.Bead{Title: "open task", Type: "task"}, "open")
	mustCreate(beads.Bead{Title: "open message", Type: "message"}, "open")
	mustCreate(beads.Bead{Title: "claimed", Type: "task"}, "in_progress")

	backing := &countingBackingStore{Store: mem, countResult: 99}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got, err := cs.Count(context.Background(), beads.ListQuery{Status: "open", AllowScan: true}, "message")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 1 {
		t.Fatalf("Count = %d, want 1 (open task, message excluded)", got)
	}
	if backing.countCalls != 0 {
		t.Fatalf("backing.Count called %d times, want 0 (cache should answer)", backing.countCalls)
	}
}

func TestCachingStoreCountDelegatesBeforePrime(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	backing := &countingBackingStore{Store: mem, countResult: 5}
	cs := beads.NewCachingStoreForTest(backing, nil)

	got, err := cs.Count(context.Background(), beads.ListQuery{Status: "open", AllowScan: true}, "message")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 5 {
		t.Fatalf("Count = %d, want 5 from backing", got)
	}
	if backing.countCalls != 1 {
		t.Fatalf("backing.Count called %d times, want 1", backing.countCalls)
	}
	if backing.gotQuery.Status != "open" || len(backing.gotExcludes) != 1 || backing.gotExcludes[0] != "message" {
		t.Fatalf("backing got query %+v excludes %v", backing.gotQuery, backing.gotExcludes)
	}
}

func TestCachingStoreCountDelegatesForClosedQueries(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	backing := &countingBackingStore{Store: mem, countResult: 12}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// The cache never holds the complete closed set; IncludeClosed counts
	// must come from the backing store.
	got, err := cs.Count(context.Background(), beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 12 {
		t.Fatalf("Count = %d, want 12 from backing", got)
	}
	if backing.countCalls != 1 {
		t.Fatalf("backing.Count called %d times, want 1", backing.countCalls)
	}
}

func TestCachingStoreCountUnsupportedWithoutBackingCounter(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	cs := beads.NewCachingStoreForTest(mem, nil)

	_, err := cs.Count(context.Background(), beads.ListQuery{AllowScan: true})
	if !errors.Is(err, beads.ErrCountUnsupported) {
		t.Fatalf("Count error = %v, want ErrCountUnsupported", err)
	}
}

func TestCachingStoreCountLiveQueryBypassesCache(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	backing := &countingBackingStore{Store: mem, countResult: 3}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got, err := cs.Count(context.Background(), beads.ListQuery{AllowScan: true, Live: true})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 3 {
		t.Fatalf("Count = %d, want 3 from backing", got)
	}
	if backing.countCalls != 1 {
		t.Fatalf("backing.Count called %d times, want 1", backing.countCalls)
	}
}
