package main

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestOrderDispatchTrackingIndexConcurrentGatesAreRaceFree reproduces the
// concurrent-map-writes crash that flaked the `cmd/gc process` shard on
// unrelated PRs (gascity#3256, gascity#3261).
//
// memoryOrderDispatcher.dispatch builds ONE shared orderDispatchTrackingIndex
// (order_dispatch.go: trackingIndex := newOrderDispatchTrackingIndex()) and
// consults it from every order's open-work gate. gateOpenWorkBounded runs each
// gate in its own goroutine and, on a per-order timeout OR ctx cancellation,
// returns WITHOUT waiting for that goroutine (by design, to avoid stalling
// later orders — #2893). The dispatch loop then spawns the next order's gate
// goroutine. Those orphaned goroutines call hasOpenTracking / lastRunFunc
// concurrently, and both write the index's unguarded `entries`/`errs` maps in
// entriesForStore and historyEntriesForStore -> "fatal error: concurrent map
// writes". On ctx cancel the loop orphans every remaining gate at once, so the
// racing writers pile up — in CI a burst of "open-work gate ... aborted:
// context canceled" immediately precedes the crash.
//
// The test hammers a single shared index from many goroutines through the same
// public entry points the dispatch loop uses (hasOpenTracking + the lastRunFunc
// closure), with a small key fan-out so reads and writes collide on the same
// map. Under `-race` the unsynchronized access is reported deterministically;
// making orderDispatchTrackingIndex guard its maps with a mutex fixes it.
func TestOrderDispatchTrackingIndexConcurrentGatesAreRaceFree(t *testing.T) {
	idx := newOrderDispatchTrackingIndex()
	stores := []beads.Store{beads.NewMemStore()}

	const (
		goroutines = 64
		keyFanout  = 8
		iterations = 16
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			// A small fan-out of shared store keys: goroutines sharing a key
			// race their cache writes, and goroutines on other keys read/write
			// the same backing map concurrently.
			storeKeys := []string{fmt.Sprintf("store-%d", g%keyFanout)}
			lastRun := idx.lastRunFunc(stores, storeKeys, nil)
			<-start // release all goroutines together to maximize overlap
			for i := 0; i < iterations; i++ {
				if _, err := idx.hasOpenTracking(stores, storeKeys, "order-x"); err != nil {
					t.Errorf("hasOpenTracking: %v", err)
					return
				}
				if _, err := lastRun("order-x"); err != nil {
					t.Errorf("lastRunFunc: %v", err)
					return
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
}
