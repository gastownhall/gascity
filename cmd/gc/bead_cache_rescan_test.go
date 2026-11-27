package main

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// rescanTestNow is the fixed instant a rescan test's cache reads as now, so
// no periodic re-scan comes due while the test runs.
var rescanTestNow = time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)

func rescanTestClock() time.Time { return rescanTestNow }

// reloadSnapshot is the registry snapshot a supervisor reload reads: views,
// in order.
func reloadSnapshot(views ...*cityView) *citySnapshot {
	return &citySnapshot{all: views}
}

// closedEvents records the bead.closed notifications a cache emits.
type closedEvents struct {
	mu  sync.Mutex
	ids []string
}

func (e *closedEvents) onChange(eventType, beadID string, _ json.RawMessage) {
	if eventType != events.BeadClosed {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, beadID)
}

func (e *closedEvents) has(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, got := range e.ids {
		if got == id {
			return true
		}
	}
	return false
}

func cachedOpenIDs(t *testing.T, cache *beads.CachingStore) map[string]bool {
	t.Helper()
	rows, ok := cache.CachedList(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if !ok {
		t.Fatal("cache does not serve its open-bead list")
	}
	ids := map[string]bool{}
	for _, b := range rows {
		ids[b.ID] = true
	}
	return ids
}

// primedRescanCache returns a cache over backing, primed at rescanTestNow
// with the given options, and the id of a bead seeded before the prime and
// then closed straight in backing, as an out-of-process bd close would.
func primedRescanCache(t *testing.T, backing beads.Store, onChange func(string, string, json.RawMessage), opts ...beads.CachingStoreOption) (*beads.CachingStore, string) {
	t.Helper()
	seeded, err := backing.Create(beads.Bead{Title: "closed by bd"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := backing.Create(beads.Bead{Title: "still open"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, onChange, append([]beads.CachingStoreOption{beads.WithClock(rescanTestClock)}, opts...)...)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(seeded.ID); err != nil {
		t.Fatalf("backing Close(%s): %v", seeded.ID, err)
	}
	return cache, seeded.ID
}

func startedCity(name string, cs *controllerState) *cityView {
	return &cityView{Name: name, Started: true, cs: cs}
}

// TestSupervisorReloadDropsOutOfProcessCloseBeforeTheCadence: a supervisor
// reload makes a cache primed less than a cadence ago re-scan at once, so a
// bead closed out of process since the prime leaves its open list (#5239).
func TestSupervisorReloadDropsOutOfProcessCloseBeforeTheCadence(t *testing.T) {
	closed := &closedEvents{}
	cache, id := primedRescanCache(t, beads.NewMemStore(), closed.onChange)
	cs := &controllerState{beadStores: map[string]beads.Store{"r": cache}, pokeCh: make(chan struct{}, 1)}

	cache.ReconcileIfDueForTest()
	if !cachedOpenIDs(t, cache)[id] {
		t.Fatalf("precondition: %s left the cache with no re-scan due", id)
	}

	pokeStartedCitiesOnReload(reloadSnapshot(startedCity("city", cs)), io.Discard)
	cache.ReconcileIfDueForTest()

	if cachedOpenIDs(t, cache)[id] {
		t.Fatalf("cache still lists %s open after a supervisor reload and a loop step", id)
	}
	if !closed.has(id) {
		t.Fatalf("cache emitted no bead.closed for %s", id)
	}
}

// TestSupervisorReloadRequestsEveryCityCacheRescan: a reload asks each
// distinct cache of every started city for one re-scan (the city cache, a
// policy-wrapped rig cache shared by two rigs and a relocated class cache),
// asks a city that is not started for none, and still wakes the allocator.
func TestSupervisorReloadRequestsEveryCityCacheRescan(t *testing.T) {
	city := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	work := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	relocated := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	idle := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	started := &controllerState{
		cityBeadStore: city,
		beadStores: map[string]beads.Store{
			"r":  &beadPolicyStore{Store: work},
			"r2": &beadPolicyStore{Store: work},
		},
		storageRoutes: splitRoutes(relocated),
		pokeCh:        make(chan struct{}, 1),
	}
	stopped := &controllerState{beadStores: map[string]beads.Store{"r": idle}, pokeCh: make(chan struct{}, 1)}

	if got := started.requestBeadCacheRescans(); got != 3 {
		t.Fatalf("requestBeadCacheRescans = %d, want 3 distinct caches", got)
	}
	before := map[string]int64{}
	for name, cache := range map[string]*beads.CachingStore{"city": city, "work": work, "relocated": relocated, "idle": idle} {
		before[name] = cache.Stats().ReconcileRequests
	}

	pokeStartedCitiesOnReload(reloadSnapshot(
		startedCity("started", started),
		&cityView{Name: "stopped", Started: false, cs: stopped},
	), io.Discard)

	for name, cache := range map[string]*beads.CachingStore{"city": city, "work": work, "relocated": relocated} {
		if got := cache.Stats().ReconcileRequests - before[name]; got != 1 {
			t.Errorf("%s cache: reload made %d reconcile requests, want 1", name, got)
		}
	}
	if got := idle.Stats().ReconcileRequests - before["idle"]; got != 0 {
		t.Errorf("a city that is not started got %d reconcile requests, want 0", got)
	}
	select {
	case <-started.pokeCh:
	default:
		t.Error("the started city got no allocator wake")
	}
	select {
	case <-stopped.pokeCh:
		t.Error("a city that is not started was woken")
	default:
	}
}
