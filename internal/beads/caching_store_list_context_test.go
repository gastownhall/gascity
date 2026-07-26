package beads_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// contextListingBackingStore is a Store + ContextLister fake that records
// ListContext calls and the ctx/query it received.
type contextListingBackingStore struct {
	beads.Store
	listContextCalls int
	gotCtx           context.Context
	gotQuery         beads.ListQuery
	result           []beads.Bead
	err              error
}

func (s *contextListingBackingStore) ListContext(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.listContextCalls++
	s.gotCtx = ctx
	s.gotQuery = query
	return s.result, s.err
}

func TestCachingStoreListContextServedFromCache(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "open task", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing := &contextListingBackingStore{Store: mem}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got, err := cs.ListContext(context.Background(), beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("ListContext: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListContext len = %d, want 1", len(got))
	}
	if backing.listContextCalls != 0 {
		t.Fatalf("backing.ListContext called %d times, want 0 (cache should answer)", backing.listContextCalls)
	}
}

func TestCachingStoreListContextDelegatesToBackingContextListerBeforePrime(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	want := []beads.Bead{{ID: "bd-1", Title: "from backing"}}
	backing := &contextListingBackingStore{Store: mem, result: want}
	cs := beads.NewCachingStoreForTest(backing, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := cs.ListContext(ctx, beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("ListContext: %v", err)
	}
	if len(got) != 1 || got[0].ID != "bd-1" {
		t.Fatalf("ListContext = %+v, want the backing's rows", got)
	}
	if backing.listContextCalls != 1 {
		t.Fatalf("backing.ListContext called %d times, want 1", backing.listContextCalls)
	}
	if backing.gotCtx != ctx {
		t.Fatal("backing.ListContext did not receive the caller's ctx")
	}
}

func TestCachingStoreListContextFallsBackToPlainListWithoutBackingContextLister(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	if _, err := mem.Create(beads.Bead{Title: "open task", Type: "task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// mem does not implement ContextLister, so CachingStore must fall back
	// to backing.List and ignore ctx, rather than erroring.
	cs := beads.NewCachingStoreForTest(mem, nil)

	got, err := cs.ListContext(context.Background(), beads.ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("ListContext: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListContext len = %d, want 1", len(got))
	}
}

func TestCachingStoreListContextLiveQueryBypassesCache(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	want := []beads.Bead{{ID: "bd-live", Title: "live row"}}
	backing := &contextListingBackingStore{Store: mem, result: want}
	cs := beads.NewCachingStoreForTest(backing, nil)
	if err := cs.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	got, err := cs.ListContext(context.Background(), beads.ListQuery{AllowScan: true, Live: true})
	if err != nil {
		t.Fatalf("ListContext: %v", err)
	}
	if len(got) != 1 || got[0].ID != "bd-live" {
		t.Fatalf("ListContext = %+v, want the backing's rows (Live bypasses cache)", got)
	}
	if backing.listContextCalls != 1 {
		t.Fatalf("backing.ListContext called %d times, want 1", backing.listContextCalls)
	}
}
