package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The controller puts the bead-policy layer back over every city and rig
// cache (wrapWithCachingStoreFeed), and the API reads the cache's age through
// that layer. Liveness must stay hidden: the API's cache_not_live gate would
// otherwise refuse city and rig reads while the cache primes.
func TestBeadPolicyStoreReportsItsCacheStatsButNotLiveness(t *testing.T) {
	primed := func(t *testing.T, backing beads.Store) *beads.CachingStore {
		t.Helper()
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(t.Context()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		if cache.Stats().LastFreshAt.IsZero() {
			t.Fatal("precondition: a primed cache has a non-zero LastFreshAt")
		}
		return cache
	}

	for _, tc := range []struct {
		name     string
		cache    func(t *testing.T) *beads.CachingStore // nil = no cache under the layer
		backing  func(t *testing.T) beads.Store
		wantType string
	}{
		{
			name:     "policy store over a cache",
			cache:    func(t *testing.T) *beads.CachingStore { return primed(t, beads.NewMemStore()) },
			wantType: "*main.beadPolicyStore",
		},
		{
			name: "policy graph store over a cache",
			cache: func(t *testing.T) *beads.CachingStore {
				return primed(t, mustOpenSQLiteStoreForCapabilityTest(t))
			},
			wantType: "*main.beadPolicyGraphStore",
		},
		{
			name:     "policy store with no cache",
			backing:  func(_ *testing.T) beads.Store { return beads.NewMemStore() },
			wantType: "*main.beadPolicyStore",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				inner beads.Store
				cache *beads.CachingStore
			)
			if tc.cache != nil {
				cache = tc.cache(t)
				inner = cache
			} else {
				inner = tc.backing(t)
			}
			wrapped := wrapStoreWithBeadPolicies(inner, &config.City{})
			if got := fmt.Sprintf("%T", wrapped); got != tc.wantType {
				t.Fatalf("wrapped type = %s, want %s", got, tc.wantType)
			}

			reporter, ok := wrapped.(interface{ Stats() beads.CacheStats })
			if !ok {
				t.Fatalf("%T does not report Stats: the API's X-GC-Cache-Age-S reads 0 for every city and rig store", wrapped)
			}
			got := reporter.Stats()
			if cache == nil {
				if got != (beads.CacheStats{}) {
					t.Errorf("Stats() over a store with no cache = %+v, want zero stats", got)
				}
			} else if want := cache.Stats().LastFreshAt; !got.LastFreshAt.Equal(want) {
				t.Errorf("Stats().LastFreshAt = %v, want the cache's %v", got.LastFreshAt, want)
			}

			if _, ok := wrapped.(interface{ IsLive() bool }); ok {
				t.Errorf("%T reports IsLive: the API's cache_not_live gate would then refuse city and rig reads (remote targets exit 1, the dashboard errors) while the cache primes", wrapped)
			}
		})
	}
}
