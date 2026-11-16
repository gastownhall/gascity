package readywork

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// ownLiveStore is a store with its own live reader, as a CachingStore and the
// bead-policy layer have: every read the federation makes must go through that
// reader. A read that reached the store's own methods instead went through the
// logical wrapper HandlesFor puts over a store with no live reader, which on a
// CachingStore is the cache itself (CachingStore.List installs and announces
// what a Live list reads; Get, Ready and DepList answer from the snapshot).
type ownLiveStore struct {
	*beads.MemStore
	direct, live atomic.Int64
}

func (s *ownLiveStore) Handles() beads.StoreHandles {
	return beads.StoreHandles{Cached: beads.HandlesFor(s.MemStore).Cached, Live: ownLiveReader{s}, Writer: s.MemStore}
}

// DepListBatch forwards no batch, as a wrapper over a store without one
// does, so edges are read per row through the live reader.
func (s *ownLiveStore) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return nil, beads.ErrDepListBatchUnsupported
}

func (s *ownLiveStore) Get(id string) (beads.Bead, error) {
	s.direct.Add(1)
	return s.MemStore.Get(id)
}

func (s *ownLiveStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.direct.Add(1)
	return s.MemStore.List(q)
}

func (s *ownLiveStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	s.direct.Add(1)
	return s.MemStore.Ready(q...)
}

func (s *ownLiveStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.direct.Add(1)
	return s.MemStore.DepList(id, direction)
}

type ownLiveReader struct{ s *ownLiveStore }

func (r ownLiveReader) Get(id string) (beads.Bead, error) {
	r.s.live.Add(1)
	return r.s.MemStore.Get(id)
}

func (r ownLiveReader) List(q beads.ListQuery) ([]beads.Bead, error) {
	r.s.live.Add(1)
	return r.s.MemStore.List(q)
}

func (r ownLiveReader) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	r.s.live.Add(1)
	return r.s.MemStore.Ready(q...)
}

func (r ownLiveReader) DepList(id, direction string) ([]beads.Dep, error) {
	r.s.live.Add(1)
	return r.s.MemStore.DepList(id, direction)
}

// handleLegQueries reach every live read the federation makes: the plain and
// narrowed ready arms (with the narrowed answer's exactness probe, which a
// work-shaped relic in the binding triggers), the bounded ready arm, the open
// and in_progress list arms, and the in_progress arm's blocker reads.
var handleLegQueries = map[string]Query{
	"ready":       {},
	"narrowed":    {Assignee: "a"},
	"bounded":     {Limit: 1},
	"open":        {Status: StatusOpen},
	"in_progress": {Status: StatusInProgress},
}

// TestLegsHoldingClassHandlesReadThroughTheStoresOwnLiveReader (typed-stores
// T6a) pins that a leg holding a class handle reads exactly as the raw leg
// does: through its store's own live reader, never its store's direct methods
// (which on a cache are the cache). Before T6a, HandlesFor on the handle found
// no live reader and read through the logical wrapper. Unsplit (one work leg)
// and split (a work leg and a class binding holding a work-shaped relic).
//
// The raw and the handle legs read the same stores. Built twice, the two
// answers could differ in order with no fault in either read: each store
// stamps its own creates strictly increasing, but a second store's first
// create takes the wall clock, which on macOS (microsecond reads) can still be
// inside the tick the first store's creates shared. The relic then sorts
// between the work rows in one build and after them in the other.
func TestLegsHoldingClassHandlesReadThroughTheStoresOwnLiveReader(t *testing.T) {
	for _, split := range []bool{false, true} {
		work := &ownLiveStore{MemStore: beads.NewMemStore()}
		row := mustCreate(t, work, beads.Bead{Title: "row", Type: "task", Assignee: "a"})
		claimed := mustCreate(t, work, beads.Bead{Title: "claimed", Type: "task", Assignee: "a"})
		inProgress := "in_progress"
		if err := work.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
		blocker := mustCreate(t, work, beads.Bead{Title: "blocker", Type: "task"})
		if err := work.DepAdd(claimed.ID, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		var binding *beads.MemStore
		if split {
			binding = beads.NewMemStore()
			binding.HonorExplicitIDs = true
			mustCreate(t, binding, beads.Bead{ID: "mc-relic", Title: "relic", Type: "task", Assignee: "a"})
		}
		answers := map[bool]map[string][]string{}
		for _, handles := range []bool{false, true} {
			var leg beads.Store = work
			if handles {
				leg = beadstest.Work(work)
			}
			legs := []Leg{testLeg("work", leg)}
			if split {
				var b beads.Store = binding
				if handles {
					b = beadstest.Graph(binding)
				}
				legs = append(legs, Leg{Label: "graph", Store: b, ClassBinding: true, ReservedPrefixes: []string{"gcg"}})
			}
			direct, live := work.direct.Load(), work.live.Load()
			answers[handles] = map[string][]string{}
			for qname, q := range handleLegQueries {
				rows, err := ReadContext(context.Background(), legs, q)
				if err != nil {
					t.Fatalf("split=%v handles=%v %s: %v", split, handles, qname, err)
				}
				for _, r := range rows {
					answers[handles][qname] = append(answers[handles][qname], r.ID)
				}
			}
			if !slices.Contains(answers[handles]["ready"], row.ID) {
				t.Errorf("split=%v handles=%v: ready = %v, want %s", split, handles, answers[handles]["ready"], row.ID)
			}
			if !slices.Contains(answers[handles]["in_progress"], claimed.ID) {
				t.Errorf("split=%v handles=%v: in_progress = %v, want %s", split, handles, answers[handles]["in_progress"], claimed.ID)
			}
			if n := work.direct.Load() - direct; n != 0 {
				t.Errorf("split=%v handles=%v: %d reads reached the store's own methods past its live reader (%d through it)", split, handles, n, work.live.Load()-live)
			}
		}
		for qname := range handleLegQueries {
			if raw, h := answers[false][qname], answers[true][qname]; !slices.Equal(raw, h) {
				t.Errorf("split=%v %s: handle legs answered %v, raw legs %v", split, qname, h, raw)
			}
		}
	}
}

// TestHandleLegsLeaveTheCacheAlone is the same pin over a real CachingStore:
// a row written to the backing store after the cache primed is in the answer,
// and reading it through a handle leg neither installs it into the cache nor
// announces it.
func TestHandleLegsLeaveTheCacheAlone(t *testing.T) {
	for _, handles := range []bool{false, true} {
		backing := beads.NewMemStore()
		mustCreate(t, backing, beads.Bead{Title: "primed", Type: "task"})
		var announced int
		cache := beads.NewCachingStoreForTest(backing, func(string, string, json.RawMessage) { announced++ })
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatal(err)
		}
		late := mustCreate(t, backing, beads.Bead{Title: "written past the cache", Type: "task"})
		var leg beads.Store = cache
		if handles {
			leg = beadstest.Work(cache)
		}
		for qname, q := range handleLegQueries {
			rows, err := ReadContext(context.Background(), []Leg{testLeg("work", leg)}, q)
			if err != nil {
				t.Fatalf("handles=%v %s: %v", handles, qname, err)
			}
			if q.Status == StatusOpen && !slices.ContainsFunc(rows, func(r ReadyBead) bool { return r.ID == late.ID }) {
				t.Errorf("handles=%v %s: the answer missed %s, written past the cache", handles, qname, late.ID)
			}
		}
		if _, err := beads.HandlesFor(cache).Cached.Get(late.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("handles=%v: the reads installed %s into the cache (%v)", handles, late.ID, err)
		}
		if announced != 0 {
			t.Errorf("handles=%v: the reads announced %d changes, want none", handles, announced)
		}
	}
}

// TestLegHelpersTakeTheBatchAndListingThroughAHandle pins the other two leg
// capabilities through a handle (T6a): legDeps reads a handle leg's edges with
// its store's one batched read, and the gate finds its store's absent-blocker
// listing, exactly as for the raw leg.
func TestLegHelpersTakeTheBatchAndListingThroughAHandle(t *testing.T) {
	for _, handles := range []bool{false, true} {
		mem := beads.NewMemStore()
		row := mustCreate(t, mem, beads.Bead{Title: "row", Type: "task"})
		blocker := mustCreate(t, mem, beads.Bead{Title: "blocker", Type: "task"})
		if err := mem.DepAdd(row.ID, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
		store := &countingBatchStore{MemStore: mem}
		var leg beads.Store = store
		if handles {
			leg = beadstest.Work(store)
		}
		deps, err := legDeps(context.Background(), testLeg("work", leg), []string{row.ID})
		if err != nil {
			t.Fatalf("handles=%v: legDeps: %v", handles, err)
		}
		if store.batches != 1 || store.perRow != 0 {
			t.Errorf("handles=%v: legDeps made %d batch and %d per-row reads, want one batch", handles, store.batches, store.perRow)
		}
		if len(deps[row.ID]) != 1 {
			t.Errorf("handles=%v: legDeps = %v, want the row's one edge", handles, deps)
		}
		if !strands(testLeg("work", leg)) {
			t.Errorf("handles=%v: the gate found no absent-blocker listing on the in-memory leg", handles)
		}
		// Without a batch the edges are read through the store's own live
		// reader, one per row.
		own := &ownLiveStore{MemStore: mem}
		var ownLeg beads.Store = own
		if handles {
			ownLeg = beadstest.Work(own)
		}
		if _, err := legDeps(context.Background(), testLeg("work", ownLeg), []string{row.ID}); err != nil {
			t.Fatalf("handles=%v: legDeps without a batch: %v", handles, err)
		}
		if own.direct.Load() != 0 || own.live.Load() != 1 {
			t.Errorf("handles=%v: legDeps without a batch made %d direct and %d live reads, want one live", handles, own.direct.Load(), own.live.Load())
		}
	}
}

// countingBatchStore counts batched and per-row edge reads.
type countingBatchStore struct {
	*beads.MemStore
	batches, perRow int
}

func (s *countingBatchStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.batches++
	out := make(map[string][]beads.Dep, len(ids))
	for _, id := range ids {
		deps, err := s.MemStore.DepList(id, "down")
		if err != nil {
			return nil, err
		}
		out[id] = deps
	}
	return out, nil
}

func (s *countingBatchStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.perRow++
	return s.MemStore.DepList(id, direction)
}
