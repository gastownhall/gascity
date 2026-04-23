package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
)

// fakeLivenessStore satisfies beads.Store by embedding a MemStore. Tests
// swap in *CachingStore separately to exercise the Live/NotLive gate.
type fakeLivenessStore struct{ beads.Store }

func TestCacheLiveOr503_NonCachingStorePasses(t *testing.T) {
	// When the handler store is not a *CachingStore (e.g., a plain
	// MemStore in tests, or a BdStore without caching wrapping), there's
	// no liveness concept to gate on — the gate is a no-op.
	mem := beads.NewMemStore()
	if err := cacheLiveOr503(fakeLivenessStore{Store: mem}); err != nil {
		t.Fatalf("cacheLiveOr503(non-caching) = %v, want nil", err)
	}
}

func TestCacheLiveOr503_NilStorePasses(t *testing.T) {
	// A nil store is treated as "no cache to gate" — the handler's own
	// nil-store guard (if any) is responsible for 503-on-no-store.
	if err := cacheLiveOr503(nil); err != nil {
		t.Fatalf("cacheLiveOr503(nil) = %v, want nil", err)
	}
}

func TestCacheLiveOr503_LiveCachePasses(t *testing.T) {
	mem := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if !cache.IsLive() {
		t.Fatalf("expected IsLive true after Prime")
	}
	if err := cacheLiveOr503(cache); err != nil {
		t.Errorf("cacheLiveOr503(live) = %v, want nil", err)
	}
}

func TestCacheLiveOr503_NotLiveReturns503(t *testing.T) {
	mem := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(mem, nil)
	// Don't call Prime; cache stays uninitialized → not live.
	if cache.IsLive() {
		t.Fatalf("expected IsLive false before Prime")
	}
	err := cacheLiveOr503(cache)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var he huma.StatusError
	if !errors.As(err, &he) {
		t.Fatalf("expected huma.StatusError, got %T: %v", err, err)
	}
	if he.GetStatus() != 503 {
		t.Errorf("status = %d, want 503", he.GetStatus())
	}
	if !strings.Contains(err.Error(), "cache_not_live") {
		t.Errorf("err = %q, want substring 'cache_not_live'", err.Error())
	}
}

func TestCacheAgeSeconds(t *testing.T) {
	mem := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Immediately after prime, age should be ~0s.
	age := cacheAgeSeconds(cache)
	if age < 0 || age > 5 {
		t.Errorf("post-prime age = %.3fs, want 0..5", age)
	}

	// Simulate time passing by manipulating via public Stats surface.
	// We can't inject a clock, so assert monotonicity.
	time.Sleep(20 * time.Millisecond)
	age2 := cacheAgeSeconds(cache)
	if age2 < age {
		t.Errorf("age decreased over time: %.6f → %.6f", age, age2)
	}
}

func TestCacheAgeSeconds_NonCachingStoreReturnsZero(t *testing.T) {
	mem := beads.NewMemStore()
	if got := cacheAgeSeconds(mem); got != 0 {
		t.Errorf("cacheAgeSeconds(non-caching) = %v, want 0", got)
	}
	if got := cacheAgeSeconds(nil); got != 0 {
		t.Errorf("cacheAgeSeconds(nil) = %v, want 0", got)
	}
}
