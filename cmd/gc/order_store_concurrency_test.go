package main

import (
	"io"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/orders"
)

// TestCachedOrderHistoryStoresResolverIsConcurrencySafe guards the contract the
// order-firing doctor check now depends on. That check resolves its per-order
// lookups in parallel, and this resolver memoises opened stores in a map; left
// unguarded that map is a data race, and under `go test -race` (or, in
// production, at random) a concurrent write panics the whole gc process.
//
// Run this with -race for it to mean anything.
func TestCachedOrderHistoryStoresResolverIsConcurrencySafe(t *testing.T) {
	cityPath := writeOrderHistoryTestCity(t)
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	resolve := cachedOrderHistoryStoresResolver(cityPath, cfg, io.Discard)

	// Distinct orders so the resolver races on inserting cache entries, not
	// just on reading one already-populated key.
	targets := []orders.Order{
		{Name: "digest"},
		{Name: "cleanup"},
		{Name: "sweep"},
		{Name: "patrol"},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	resolved := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores, err := resolve(targets[i%len(targets)])
			if err != nil || len(stores) == 0 {
				return
			}
			mu.Lock()
			resolved++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Self-validation: if nothing resolved, the goroutines never reached the
	// memo map and the test would pass without exercising the race at all.
	if resolved == 0 {
		t.Fatal("no concurrent resolution succeeded; the test never exercised the store cache")
	}
}
