package beads

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// Repro: a full Prime over a populated cache replaces c.beads with the
// active-only snapshot (cacheFullScanQuery, IncludeClosed: false). A row the
// cache held open that was closed behind it is absent from that snapshot, so
// the replace drops it without a notification, and no later reconcile can
// announce it because the cache no longer holds it. The reconcile path, in
// contrast, re-verifies a missing row and announces a synthetic bead.closed.

type reproCloseRecorder struct {
	mu     sync.Mutex
	events []string
	closed map[string]int
}

func (r *reproCloseRecorder) onChange(eventType, beadID string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventType+" "+beadID)
	if eventType == "bead.closed" {
		r.closed[beadID]++
	}
}

func (r *reproCloseRecorder) snapshot() (map[string]int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string]int, len(r.closed))
	for k, v := range r.closed {
		counts[k] = v
	}
	return counts, append([]string(nil), r.events...)
}

func reproHolds(cs *CachingStore, id string) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	_, ok := cs.beads[id]
	return ok
}

func TestReproPrimeReplaceDropsExternalCloseSilently(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		first func(*CachingStore) error // fills the cache with the open row
		then  func(*CachingStore) error // sees the cache after the close behind it
	}{
		// The controller's startup order: PrimeActive synchronously, then
		// an async full Prime (cmd/gc/api_state.go primeThenStartReconciler).
		{
			"PrimeActiveThenPrime", func(cs *CachingStore) error { return cs.PrimeActive() },
			func(cs *CachingStore) error { return cs.Prime(ctx) },
		},
		// A second full Prime over a live cache.
		{
			"PrimeThenPrime", func(cs *CachingStore) error { return cs.Prime(ctx) },
			func(cs *CachingStore) error { return cs.Prime(ctx) },
		},
		// Control: the same close seen by a reconcile pass instead.
		{
			"PrimeActiveThenReconcile_control", func(cs *CachingStore) error { return cs.PrimeActive() },
			func(cs *CachingStore) error { cs.ReconcileNowForTest(); return nil },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemStore()
			seed, err := mem.Create(Bead{Title: "closed behind the cache"})
			if err != nil {
				t.Fatalf("Create seed: %v", err)
			}
			bystander, err := mem.Create(Bead{Title: "stays open"})
			if err != nil {
				t.Fatalf("Create bystander: %v", err)
			}
			rec := &reproCloseRecorder{closed: map[string]int{}}
			cs := NewCachingStoreForTest(mem, rec.onChange)
			if err := tc.first(cs); err != nil {
				t.Fatalf("first: %v", err)
			}
			if !reproHolds(cs, seed.ID) {
				t.Fatalf("cache does not hold %s open before the close; the case is vacuous", seed.ID)
			}

			// Another process closes the bead in the shared store.
			if err := mem.Close(seed.ID); err != nil {
				t.Fatalf("backing Close: %v", err)
			}
			if err := tc.then(cs); err != nil {
				t.Fatalf("then: %v", err)
			}
			t.Logf("after %s: cache holds %s = %v", tc.name, seed.ID, reproHolds(cs, seed.ID))

			// Settle as the conformance suite does: a live list that includes
			// closed rows, then two reconcile passes.
			if _, err := cs.List(ListQuery{AllowScan: true, IncludeClosed: true, Live: true}); err != nil {
				t.Fatalf("live List: %v", err)
			}
			cs.ReconcileNowForTest()
			cs.ReconcileNowForTest()

			counts, events := rec.snapshot()
			t.Logf("notifications: %v", events)
			if got, err := cs.Get(seed.ID); err != nil || got.Status != "closed" {
				t.Errorf("Get(%s) = (%q, %v), want closed", seed.ID, got.Status, err)
			}
			if counts[bystander.ID] != 0 {
				t.Errorf("bead.closed for open bystander %s = %d, want 0", bystander.ID, counts[bystander.ID])
			}
			if counts[seed.ID] != 1 {
				t.Errorf("bead.closed for %s = %d, want exactly 1 (closed behind the cache, then seen by %s)", seed.ID, counts[seed.ID], tc.name)
			}
		})
	}
}
