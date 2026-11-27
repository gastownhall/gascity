package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
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

// rescanListStore counts the backing List calls a cache makes and, while
// armed, blocks each one until release is closed.
type rescanListStore struct {
	beads.Store
	lists   atomic.Int32
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *rescanListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists.Add(1)
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	return s.Store.List(query)
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

// TestSupervisorReloadRescanWaitsForAClosedGate: a reload does not read a
// quiescent city's or a suspended rig's store (#6561). The request waits for
// the gate, and the first loop step after it reopens runs one scan.
func TestSupervisorReloadRescanWaitsForAClosedGate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(cs *controllerState)
		open  func(cs *controllerState)
	}{
		{
			name:  "quiescent city",
			close: func(cs *controllerState) { cs.beadsQuiescent.Store(true) },
			open:  func(cs *controllerState) { cs.beadsQuiescent.Store(false) },
		},
		{
			name:  "suspended rig",
			close: func(cs *controllerState) { cs.suspendedRigs.Store(&map[string]bool{"r": true}) },
			open:  func(cs *controllerState) { cs.suspendedRigs.Store(&map[string]bool{}) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := &controllerState{beadsQuiescent: new(atomic.Bool), pokeCh: make(chan struct{}, 1)}
			backing := &rescanListStore{Store: beads.NewMemStore()}
			cache, id := primedRescanCache(t, backing, nil, cs.rigReconcileGate("r"))
			cs.beadStores = map[string]beads.Store{"r": cache}
			tc.close(cs)
			lists := backing.lists.Load()

			pokeStartedCitiesOnReload(reloadSnapshot(startedCity("city", cs)), io.Discard)
			cache.ReconcileIfDueForTest()
			if got := backing.lists.Load() - lists; got != 0 {
				t.Fatalf("closed gate: backing listed %d times, want 0", got)
			}
			if !cachedOpenIDs(t, cache)[id] {
				t.Fatalf("closed gate: %s left the cache with no scan", id)
			}

			tc.open(cs)
			cache.ReconcileIfDueForTest()
			cache.ReconcileIfDueForTest()
			if got := backing.lists.Load() - lists; got != 1 {
				t.Fatalf("reopened gate: backing listed %d times over two loop steps, want 1", got)
			}
			if cachedOpenIDs(t, cache)[id] {
				t.Fatalf("reopened gate: cache still lists %s open", id)
			}
		})
	}
}

// TestSupervisorReloadDoesNotWaitForAScan: the per-city reload poke returns
// while a cache's scan is blocked in its backing List, and the request it
// made gets one scan of its own once that scan is released.
func TestSupervisorReloadDoesNotWaitForAScan(t *testing.T) {
	backing := &rescanListStore{Store: beads.NewMemStore(), entered: make(chan struct{}), release: make(chan struct{})}
	cache, _ := primedRescanCache(t, backing, nil)
	cs := &controllerState{beadStores: map[string]beads.Store{"r": cache}, pokeCh: make(chan struct{}, 1)}

	backing.armed.Store(true)
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		cache.ReconcileNowForTest()
	}()
	<-backing.entered

	poked := make(chan struct{})
	go func() {
		defer close(poked)
		pokeStartedCitiesOnReload(reloadSnapshot(startedCity("city", cs)), io.Discard)
	}()
	select {
	case <-poked:
	case <-time.After(time.Second):
		close(backing.release)
		t.Fatal("the reload poke waited on a blocked scan")
	}

	close(backing.release)
	<-scanned
	lists := backing.lists.Load()
	cache.ReconcileIfDueForTest()
	cache.ReconcileIfDueForTest()
	if got := backing.lists.Load() - lists; got != 1 {
		t.Fatalf("after the blocked scan: backing listed %d times over two loop steps, want 1 for the reload's request", got)
	}
}

// TestSupervisorReloadHelpMentionsBeadCacheRescan: the reload help says it
// also re-scans the bead caches GET /beads serves, that the re-scan
// finishes after the reply, and that every supervisor reload, the ones gc
// sends on its own included, requests it.
func TestSupervisorReloadHelpMentionsBeadCacheRescan(t *testing.T) {
	var out bytes.Buffer
	cmd := newSupervisorReloadCmd(io.Discard, io.Discard)
	cmd.SetOut(&out)
	if err := cmd.Help(); err != nil {
		t.Fatalf("Help: %v", err)
	}
	help := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"requests an immediate re-scan of every running city's bead caches",
		"GET /beads",
		"after the reload replies",
		"Every supervisor reload requests this re-scan, including the reloads gc sends on its own",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("gc supervisor reload --help does not say %q:\n%s", want, out.String())
		}
	}
	if !strings.Contains(cmd.Short, "bead caches") {
		t.Errorf("Short = %q; want it to name the bead caches", cmd.Short)
	}
}
