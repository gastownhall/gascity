package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
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
	// Deterministic against a real CachingStore: freeze the clock a fixed
	// interval past the primed LastFreshAt and assert the exact age. (Before
	// clock injection this test could only assert monotonicity.)
	mem := beads.NewMemStore()
	cache := beads.NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	lastFresh := cache.Stats().LastFreshAt
	if lastFresh.IsZero() {
		t.Fatal("expected non-zero LastFreshAt after Prime")
	}
	restore := SetLivenessClockForTest(&clock.Fake{Time: lastFresh.Add(12 * time.Second)})
	defer restore()
	if got := cacheAgeSeconds(cache); got != 12 {
		t.Errorf("cacheAgeSeconds = %v, want exactly 12", got)
	}
}

// stubLivenessReporter is a fully controllable livenessReporter for the
// cache-age conformance lane. It embeds a nil beads.Store so it satisfies the
// Store type cacheAgeSeconds/cacheLiveOr503 accept; only the two liveness
// methods those helpers actually call are implemented.
type stubLivenessReporter struct {
	beads.Store
	live      bool
	lastFresh time.Time
}

func (s stubLivenessReporter) IsLive() bool { return s.live }
func (s stubLivenessReporter) Stats() beads.CacheStats {
	return beads.CacheStats{LastFreshAt: s.lastFresh}
}

func TestCacheAgeSeconds_ClockInjectedStates(t *testing.T) {
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	restore := SetLivenessClockForTest(&clock.Fake{Time: base})
	defer restore()

	for _, tc := range []struct {
		name  string
		store beads.Store
		want  float64
	}{
		{"live-2s", stubLivenessReporter{live: true, lastFresh: base.Add(-2 * time.Second)}, 2},
		{"lagging-35s-past-banner", stubLivenessReporter{live: true, lastFresh: base.Add(-35 * time.Second)}, 35},
		{"priming-never-fresh", stubLivenessReporter{live: false, lastFresh: time.Time{}}, 0},
		{"clock-skew-negative-clamped", stubLivenessReporter{live: true, lastFresh: base.Add(5 * time.Second)}, 0},
		{"non-caching", beads.NewMemStore(), 0},
		{"nil-store", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheAgeSeconds(tc.store); got != tc.want {
				t.Errorf("cacheAgeSeconds(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestCacheLiveOr503_StubStates(t *testing.T) {
	if err := cacheLiveOr503(stubLivenessReporter{live: true}); err != nil {
		t.Errorf("live stub = %v, want nil", err)
	}
	err := cacheLiveOr503(stubLivenessReporter{live: false})
	if err == nil || !strings.Contains(err.Error(), "cache_not_live") {
		t.Errorf("not-live stub = %v, want cache_not_live 503", err)
	}
}

func TestSetLivenessClockForTest_Restores(t *testing.T) {
	before := livenessClock
	restore := SetLivenessClockForTest(&clock.Fake{Time: time.Unix(0, 0)})
	if livenessClock == before {
		t.Fatal("SetLivenessClockForTest did not swap the clock")
	}
	restore()
	if livenessClock != before {
		t.Fatal("restore did not put the original clock back")
	}
}
