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
