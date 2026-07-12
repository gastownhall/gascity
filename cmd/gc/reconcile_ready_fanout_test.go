package main

import (
	"errors"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// readyPartialLiveStore returns its rows alongside a PartialResultError, to
// exercise the cached controller-demand read's tier-merge branch.
type readyPartialLiveStore struct {
	beads.Store
	rows []beads.Bead
}

func (s *readyPartialLiveStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	out := append([]beads.Bead(nil), s.rows...)
	return out, &beads.PartialResultError{Op: "bd ready", Err: errors.New("skipped corrupt bead")}
}

// seedReadyWork creates an open, unblocked work bead assigned to assignee.
func seedReadyWork(t *testing.T, store beads.Store, title, assignee string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    title,
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
	})
	if err != nil {
		t.Fatalf("create ready work %q: %v", title, err)
	}
	return b
}

func readyIDs(rows []beads.Bead) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestReadyDemandCacheCollapsesReadyFanout proves the per-pass cache turns the
// N-assignee live-Ready fan-out (plus the scale-check and named-session probes)
// into at most one backing read per tier for a single store, instead of one
// read per assignee/probe. This is the core of the reconcile-tick perf fix:
// before the cache one pass issued ~60 sequential /beads/ready reads.
func TestReadyDemandCacheCollapsesReadyFanout(t *testing.T) {
	store := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	for _, assignee := range []string{"worker-a", "worker-b", "worker-c", "worker-d"} {
		seedReadyWork(t, store, "work for "+assignee, assignee)
	}

	cache := newReadyDemandCache()

	// Assigned-work probe: one live read per assignee in the legacy path.
	for _, assignee := range []string{"worker-a", "worker-b", "worker-c", "worker-d"} {
		if _, err := cache.liveReady(store, beads.ReadyQuery{Assignee: assignee, Limit: 5}); err != nil {
			t.Fatalf("liveReady(%q): %v", assignee, err)
		}
	}
	// Assigned-work no-assignee probe.
	if _, err := cache.liveReady(store, beads.ReadyQuery{Limit: 5}); err != nil {
		t.Fatalf("liveReady(no assignee): %v", err)
	}
	// Scale-check + named-session probes: full ready set, repeated per group.
	for i := 0; i < 3; i++ {
		if _, err := cache.controllerDemandReady(store); err != nil {
			t.Fatalf("controllerDemandReady #%d: %v", i, err)
		}
	}

	// A plain store has no explicit cached/live split, so both the live snapshot
	// and the cached snapshot resolve to store.Ready — at most two backing reads
	// total, regardless of how many assignees or probe groups asked.
	if got := len(store.readyQueries); got > 2 {
		t.Fatalf("backing Ready reads = %d, want <= 2 for a single store across the whole demand phase: %#v", got, store.readyQueries)
	}
}

// TestReadyDemandCacheLiveReadyEquivalentToDirect proves the snapshot-filtered
// live read returns exactly what a direct assignee/limit-scoped Ready would, so
// the demand probes see the same beads they see today.
func TestReadyDemandCacheLiveReadyEquivalentToDirect(t *testing.T) {
	seed := func(store beads.Store) {
		seedReadyWork(t, store, "a1", "worker-a")
		seedReadyWork(t, store, "a2", "worker-a")
		seedReadyWork(t, store, "b1", "worker-b")
		seedReadyWork(t, store, "unassigned", "")
	}
	cached := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	oracle := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	seed(cached)
	seed(oracle)

	cache := newReadyDemandCache()
	queries := []beads.ReadyQuery{
		{},
		{Limit: 1},
		{Assignee: "worker-a"},
		{Assignee: "worker-a", Limit: 1},
		{Assignee: "worker-b", Limit: 5},
		{Assignee: "missing"},
	}
	for _, q := range queries {
		want, err := liveReadyForControllerDemandQuery(oracle, q)
		if err != nil {
			t.Fatalf("oracle liveReady %+v: %v", q, err)
		}
		got, err := cache.liveReady(cached, q)
		if err != nil {
			t.Fatalf("cache liveReady %+v: %v", q, err)
		}
		wantIDs := readyIDs(want)
		gotIDs := readyIDs(got)
		if len(wantIDs) != len(gotIDs) {
			t.Fatalf("liveReady %+v returned %v, want %v", q, gotIDs, wantIDs)
		}
		for i := range wantIDs {
			if wantIDs[i] != gotIDs[i] {
				t.Fatalf("liveReady %+v returned %v, want %v", q, gotIDs, wantIDs)
			}
		}
	}
}

// TestReadyDemandCacheControllerDemandEquivalentToDirect proves the cached
// controller-demand read matches the free function on both a plain store and a
// CachingStore-backed store (explicit cached/live handles + merge path).
func TestReadyDemandCacheControllerDemandEquivalentToDirect(t *testing.T) {
	t.Run("plain store", func(t *testing.T) {
		cached := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
		oracle := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
		for _, s := range []beads.Store{cached, oracle} {
			seedReadyWork(t, s, "w1", "worker-a")
			seedReadyWork(t, s, "w2", "")
		}
		want, err := readyForControllerDemand(oracle)
		if err != nil {
			t.Fatalf("oracle readyForControllerDemand: %v", err)
		}
		got, err := newReadyDemandCache().controllerDemandReady(cached)
		if err != nil {
			t.Fatalf("cache controllerDemandReady: %v", err)
		}
		if a, b := readyIDs(want), readyIDs(got); len(a) != len(b) {
			t.Fatalf("controllerDemandReady returned %v, want %v", b, a)
		}
	})

	t.Run("caching store", func(t *testing.T) {
		build := func() *beads.CachingStore {
			backing := beads.NewMemStore()
			if _, err := backing.Create(beads.Bead{Title: "routed", Type: "task", Status: "open"}); err != nil {
				t.Fatalf("seed backing: %v", err)
			}
			c := beads.NewCachingStoreForTest(backing, nil)
			if err := c.PrimeActive(); err != nil {
				t.Fatalf("PrimeActive: %v", err)
			}
			return c
		}
		oracle := build()
		cached := build()
		want, err := readyForControllerDemand(oracle)
		if err != nil {
			t.Fatalf("oracle readyForControllerDemand: %v", err)
		}
		got, err := newReadyDemandCache().controllerDemandReady(cached)
		if err != nil {
			t.Fatalf("cache controllerDemandReady: %v", err)
		}
		if a, b := readyIDs(want), readyIDs(got); len(a) != len(b) {
			t.Fatalf("controllerDemandReady returned %v, want %v", b, a)
		}
	})

	t.Run("explicit handles partial live merge", func(t *testing.T) {
		cachedRows := []beads.Bead{{ID: "bd-cached", Status: "open"}}
		liveRows := []beads.Bead{{ID: "bd-live", Status: "open"}}
		build := func() beads.Store {
			return controllerDemandHandlesStore{
				Store: beads.NewMemStore(),
				handles: beads.StoreHandles{
					Cached: &readyStaticStore{ready: cachedRows},
					Live:   &readyPartialLiveStore{rows: liveRows},
				},
			}
		}
		want, wantErr := readyForControllerDemandQuery(build(), beads.ReadyQuery{})
		got, gotErr := newReadyDemandCache().controllerDemandReady(build())
		if (wantErr == nil) != (gotErr == nil) || beads.IsPartialResult(wantErr) != beads.IsPartialResult(gotErr) {
			t.Fatalf("controllerDemandReady err = %v, want %v", gotErr, wantErr)
		}
		a, b := readyIDs(want), readyIDs(got)
		if len(a) != len(b) {
			t.Fatalf("controllerDemandReady merged rows = %v, want %v", b, a)
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("controllerDemandReady merged rows = %v, want %v", b, a)
			}
		}
	})
}

// TestCollectAssignedWorkBeadsCachedMatchesUncached proves that threading a
// shared cache through the assigned-work collection returns the same beads and
// readiness verdicts as the legacy per-assignee fan-out, while collapsing the
// N-assignee live reads to a single backing read.
func TestCollectAssignedWorkBeadsCachedMatchesUncached(t *testing.T) {
	seed := func(store *readyQueryRecordingStore) *sessionBeadSnapshot {
		var sessions []beads.Bead
		for _, name := range []string{"worker-a", "worker-b", "worker-c"} {
			s, err := store.Create(beads.Bead{
				Title:  name + " session",
				Type:   sessionBeadType,
				Status: "open",
				Metadata: map[string]string{
					"session_name": name,
					"template":     "worker",
					"state":        "asleep",
				},
			})
			if err != nil {
				t.Fatalf("create session %q: %v", name, err)
			}
			sessions = append(sessions, s)
			seedReadyWork(t, store, "ready for "+name, name)
		}
		return newSessionBeadSnapshot(sessions)
	}

	uncachedStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	uncachedSnap := seed(uncachedStore)
	wantBeads, _, _, wantReady, wantPartial := collectAssignedWorkBeadsWithStores(&config.City{}, uncachedStore, nil, nil, uncachedSnap)

	cachedStore := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	cachedSnap := seed(cachedStore)
	cache := newReadyDemandCache()
	gotBeads, _, _, gotReady, gotPartial := collectAssignedWorkBeadsWithStores(&config.City{}, cachedStore, nil, nil, cachedSnap, cache)

	if wantPartial != gotPartial {
		t.Fatalf("partial mismatch: uncached=%v cached=%v", wantPartial, gotPartial)
	}
	if a, b := readyIDs(wantBeads), readyIDs(gotBeads); len(a) != len(b) {
		t.Fatalf("assigned work mismatch: cached=%v uncached=%v", b, a)
	} else {
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("assigned work mismatch: cached=%v uncached=%v", b, a)
			}
		}
	}
	if len(wantReady) != len(gotReady) {
		t.Fatalf("readyAssigned mismatch: cached=%v uncached=%v", gotReady, wantReady)
	}
	for k := range wantReady {
		if !gotReady[k] {
			t.Fatalf("readyAssigned missing %+v in cached path: %v", k, gotReady)
		}
	}

	// Legacy path probes one live read per assignee; the cached path collapses
	// them to one backing read for the single store.
	uncachedReadyReads := len(uncachedStore.readyQueries)
	cachedReadyReads := len(cachedStore.readyQueries)
	if uncachedReadyReads < 3 {
		t.Fatalf("expected legacy per-assignee fan-out (>=3 reads), got %d", uncachedReadyReads)
	}
	if cachedReadyReads > 1 {
		t.Fatalf("cached assigned-work path issued %d backing Ready reads, want <= 1", cachedReadyReads)
	}
}
