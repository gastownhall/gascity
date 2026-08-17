package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// recordingWorkStore embeds a MemStore (so it satisfies the full beads.Store
// interface) and records every List/Ready query the work-assignment façade
// emits, while delegating to the MemStore for real results. It deliberately
// does NOT implement CachedList or Backing(): the façade's optional-capability
// fast-paths must degrade to the plain List/Ready calls, exactly as a
// non-caching store does today.
type recordingWorkStore struct {
	*beads.MemStore
	listQueries  []beads.ListQuery
	readyQueries []beads.ReadyQuery
}

func newRecordingWorkStore() *recordingWorkStore {
	return &recordingWorkStore{MemStore: beads.NewMemStore()}
}

func (s *recordingWorkStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listQueries = append(s.listQueries, q)
	return s.MemStore.List(q)
}

func (s *recordingWorkStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	if len(q) > 0 {
		s.readyQueries = append(s.readyQueries, q[0])
	} else {
		s.readyQueries = append(s.readyQueries, beads.ReadyQuery{})
	}
	return s.MemStore.Ready(q...)
}

// TestWorkAssignmentOpenAssignedTo_ByteIdenticalQuery asserts the façade's
// OpenAssignedTo emits the exact same ListQuery the raw probe ran:
// {Assignee,Status,Live,TierMode}, against the WORK store.
func TestWorkAssignmentOpenAssignedTo_ByteIdenticalQuery(t *testing.T) {
	rec := newRecordingWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if _, err := wa.OpenAssignedTo("agent-1", "in_progress", beads.TierBoth, true); err != nil {
		t.Fatalf("OpenAssignedTo: %v", err)
	}

	want := beads.ListQuery{Assignee: "agent-1", Status: "in_progress", Live: true, TierMode: beads.TierBoth}
	if len(rec.listQueries) != 1 {
		t.Fatalf("expected exactly 1 List call, got %d: %#v", len(rec.listQueries), rec.listQueries)
	}
	if !reflect.DeepEqual(rec.listQueries[0], want) {
		t.Fatalf("List query mismatch:\n got  %#v\n want %#v", rec.listQueries[0], want)
	}
}

// TestWorkAssignmentReadyAssignedTo_ByteIdenticalQuery asserts ReadyAssignedTo
// emits the same ReadyQuery{Assignee,TierMode} the raw beads.ReadyLive probe ran.
func TestWorkAssignmentReadyAssignedTo_ByteIdenticalQuery(t *testing.T) {
	rec := newRecordingWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if _, err := wa.ReadyAssignedTo("agent-2", beads.TierWisps); err != nil {
		t.Fatalf("ReadyAssignedTo: %v", err)
	}

	want := beads.ReadyQuery{Assignee: "agent-2", TierMode: beads.TierWisps}
	if len(rec.readyQueries) != 1 {
		t.Fatalf("expected exactly 1 Ready call, got %d: %#v", len(rec.readyQueries), rec.readyQueries)
	}
	if !reflect.DeepEqual(rec.readyQueries[0], want) {
		t.Fatalf("Ready query mismatch:\n got  %#v\n want %#v", rec.readyQueries[0], want)
	}
}

// TestWorkAssignmentCachedOpenAssignedWisps_NoFastPathWithoutCache asserts that
// on a store without the CachedList capability the cache probe reports "not
// answered" (so the caller falls through to OpenAssignedTo), and crucially does
// NOT assert CachedList on the WorkStore wrapper (which would always fail and
// silently drop the cache on a real caching store).
func TestWorkAssignmentCachedOpenAssignedWisps_NoFastPathWithoutCache(t *testing.T) {
	rec := newRecordingWorkStore()
	wa := workAssignmentForStore(beads.WorkStore{Store: rec})

	if items, ok := wa.CachedOpenAssignedWisps("agent-3", "open"); ok {
		t.Fatalf("expected cache miss on non-caching store, got ok=true items=%#v", items)
	}
	if len(rec.listQueries) != 0 {
		t.Fatalf("cache probe must not issue a List on a non-caching store, got %#v", rec.listQueries)
	}
}

// fakeCachingWorkStore implements the CachedList capability on the underlying
// store (not the wrapper) so the façade's fast-path can be exercised.
type fakeCachingWorkStore struct {
	*beads.MemStore
	cachedCalls []beads.ListQuery
	cachedHit   []beads.Bead
	cachedOK    bool
}

func (s *fakeCachingWorkStore) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	s.cachedCalls = append(s.cachedCalls, q)
	return s.cachedHit, s.cachedOK
}

// TestWorkAssignmentCachedOpenAssignedWisps_UsesUnwrappedStore proves the
// CachedList assertion is made on the embedded .Store, not the WorkStore
// wrapper: the wrapper does not promote CachedList, so asserting on it would
// miss this capability and silently lose the fast-path (the typed-nil trap).
func TestWorkAssignmentCachedOpenAssignedWisps_UsesUnwrappedStore(t *testing.T) {
	want := beads.ListQuery{Assignee: "agent-4", Status: "in_progress", TierMode: beads.TierWisps}
	cache := &fakeCachingWorkStore{
		MemStore:  beads.NewMemStore(),
		cachedHit: []beads.Bead{{ID: "w-1"}},
		cachedOK:  true,
	}
	wa := workAssignmentForStore(beads.WorkStore{Store: cache})

	items, ok := wa.CachedOpenAssignedWisps("agent-4", "in_progress")
	if !ok {
		t.Fatalf("expected cache hit via unwrapped .Store, got ok=false")
	}
	if len(items) != 1 || items[0].ID != "w-1" {
		t.Fatalf("unexpected cached items: %#v", items)
	}
	if len(cache.cachedCalls) != 1 || !reflect.DeepEqual(cache.cachedCalls[0], want) {
		t.Fatalf("CachedList query mismatch:\n got %#v\n want %#v", cache.cachedCalls, want)
	}
}

// TestWorkAssignmentForStore_NilUnderlyingStoreSafe asserts the façade tolerates
// a nil underlying store the same way the raw probes did (return empty, no
// panic).
func TestWorkAssignmentForStore_NilUnderlyingStoreSafe(t *testing.T) {
	wa := workAssignmentForStore(beads.WorkStore{Store: nil})
	if items, err := wa.OpenAssignedTo("a", "open", beads.TierBoth, true); err != nil || items != nil {
		t.Fatalf("nil store OpenAssignedTo: items=%#v err=%v", items, err)
	}
	if items, err := wa.ReadyAssignedTo("a", beads.TierIssues); err != nil || items != nil {
		t.Fatalf("nil store ReadyAssignedTo: items=%#v err=%v", items, err)
	}
	if items, ok := wa.CachedOpenAssignedWisps("a", "open"); ok || items != nil {
		t.Fatalf("nil store CachedOpenAssignedWisps: items=%#v ok=%v", items, ok)
	}
}
