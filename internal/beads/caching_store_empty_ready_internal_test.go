package beads

import (
	"context"
	"errors"
	"testing"
)

// An empty default work ledger has no dependency edge that could establish
// BdStore's inline-projection witness. Its full, clean snapshot nevertheless
// proves there is no ready candidate, without vouching for any row's edges.
func TestCachingStoreReadyContextEmptyBdSnapshot(t *testing.T) {
	ledger := &fakeBdLedger{}
	cache, store := primedBdCache(t, ledger)
	if store.listIncludesCompleteDependencies() {
		t.Fatal("empty ledger must not establish an inline dependency witness")
	}
	cache.mu.RLock()
	state, count, complete := cache.state, len(cache.beads), cache.depsComplete
	cache.mu.RUnlock()
	if state != cacheLive || count != 0 || complete {
		t.Fatalf("fixture = state %v, rows %d, depsComplete %v; want full empty snapshot with incomplete dependency witness", state, count, complete)
	}
	ledger.mu.Lock()
	calls := len(ledger.calls)
	ledger.mu.Unlock()
	for range 3 {
		rows, err := cache.ReadyContext(context.Background())
		if err != nil || len(rows) != 0 {
			t.Fatalf("ReadyContext = %+v, %v; want authoritative empty rows", rows, err)
		}
	}
	ledger.mu.Lock()
	after := len(ledger.calls)
	ledger.mu.Unlock()
	if after != calls {
		t.Fatalf("cache-only reads spent %d bd calls, want none", after-calls)
	}
	cache.mu.RLock()
	complete = cache.depsComplete
	cache.mu.RUnlock()
	if complete || store.listIncludesCompleteDependencies() {
		t.Fatal("empty readiness must not claim dependency completeness")
	}
}

func TestCachingStoreReadyContextEmptySnapshotKeepsUncertaintyGuards(t *testing.T) {
	cases := []struct {
		name   string
		change func(*CachingStore)
	}{
		{"nonempty incomplete", func(c *CachingStore) {
			c.beads["work"] = Bead{ID: "work", Type: "task", Status: "open"}
		}},
		{"dirty empty", func(c *CachingStore) { c.dirty["work"] = struct{}{} }},
		{"partial active snapshot", func(c *CachingStore) { c.state = cachePartial }},
		{"uninitialized", func(c *CachingStore) { c.state = cacheUninitialized }},
		{"partial listing", func(c *CachingStore) { c.primePartialErr = errors.New("incomplete list") }},
		{"degraded projection", func(c *CachingStore) { c.readyProjectionDegraded.Store(true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache, _ := primedBdCache(t, &fakeBdLedger{})
			cache.mu.Lock()
			tc.change(cache)
			cache.mu.Unlock()
			rows, err := cache.ReadyContext(context.Background())
			if !errors.Is(err, ErrCacheUnavailable) || len(rows) != 0 {
				t.Fatalf("ReadyContext = %+v, %v; want no rows and ErrCacheUnavailable", rows, err)
			}
		})
	}
	t.Run("canceled context", func(t *testing.T) {
		cache, _ := primedBdCache(t, &fakeBdLedger{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rows, err := cache.ReadyContext(ctx)
		if !errors.Is(err, context.Canceled) || len(rows) != 0 {
			t.Fatalf("ReadyContext = %+v, %v; want no rows and context.Canceled", rows, err)
		}
	})
}
