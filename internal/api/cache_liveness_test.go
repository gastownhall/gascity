package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
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

// statsOnlyStore reports cache stats but no liveness: the shape the
// controller's bead-policy layer presents over a city or rig cache.
type statsOnlyStore struct {
	beads.Store
	lastFresh time.Time
}

func (s statsOnlyStore) Stats() beads.CacheStats {
	return beads.CacheStats{LastFreshAt: s.lastFresh}
}

func TestCacheAgeSecondsReadsAStoreThatReportsStatsWithoutLiveness(t *testing.T) {
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	restore := SetLivenessClockForTest(&clock.Fake{Time: base})
	defer restore()

	store := statsOnlyStore{lastFresh: base.Add(-45 * time.Second)}
	if got := cacheAgeSeconds(store); got != 45 {
		t.Errorf("cacheAgeSeconds(stats-only store) = %v, want 45: the age must not depend on the store also reporting liveness", got)
	}
	if err := cacheLiveOr503(store); err != nil {
		t.Errorf("cacheLiveOr503(stats-only store) = %v, want nil: a store without IsLive is never gated", err)
	}
}

// cacheAgeOf GETs path from state's city through h and returns the
// X-GC-Cache-Age-S it answers, 0 when the header is absent.
func cacheAgeOf(t *testing.T, h http.Handler, state State, path string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, path), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("X-GC-Cache-Age-S")
	if raw == "" {
		return 0
	}
	age, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("GET %s: X-GC-Cache-Age-S = %q: %v", path, raw, err)
	}
	return age
}

// backdateCachedResponse moves the build time of srv's response cached under
// key d into the past.
func backdateCachedResponse(t *testing.T, srv *Server, key string, d time.Duration) {
	t.Helper()
	srv.responseCacheMu.Lock()
	defer srv.responseCacheMu.Unlock()
	entry, ok := srv.responseCacheEntries[key]
	if !ok {
		t.Fatalf("no response cached under %q", key)
	}
	entry.storedAt = entry.storedAt.Add(-d)
	srv.responseCacheEntries[key] = entry
}

// setCitySuspendedForTest writes the city's runtime suspension state as gc
// suspend and gc resume do.
func setCitySuspendedForTest(t *testing.T, cityPath string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, &suspended); err != nil {
		t.Fatal(err)
	}
}

// GET /rigs and GET /maintenance/status report no cache age, however old the
// city store's cache is. Their bodies come from config, the suspension state,
// runtime probes and the supervisor's maintenance loop, not from the bead
// cache, so its age says nothing about them, and a CLI that saw it would print
// the "reconciler may be lagging" footer over output that never came from the
// cache. Neither does GET /status on a suspended city, whose body is built
// without reading any bead store. GET /beads, which is read from the cache, is
// the control: it reports the same store's age, as GET /status does once the
// city is resumed.
func TestReadsNotServedFromTheBeadCacheReportNoCacheAge(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))

	fs := maintenanceTestState(t, &fakeMaintenanceLoop{})
	fs.cityBeadStore = stubLivenessReporter{Store: beads.NewMemStore(), live: true, lastFresh: now.Add(-45 * time.Second)}
	h := newTestCityHandler(t, fs)

	if age := cacheAgeOf(t, h, fs, "/beads"); age != 45 {
		t.Fatalf("precondition: GET /beads X-GC-Cache-Age-S = %v, want 45, the city store's cache age", age)
	}
	for _, path := range []string{"/rigs", "/maintenance/status"} {
		if age := cacheAgeOf(t, h, fs, path); age != 0 {
			t.Errorf("GET %s X-GC-Cache-Age-S = %v, want 0: its body is not read from the bead cache", path, age)
		}
	}

	// Each /status read goes through a new handler, whose server has no /status
	// body cached from the other suspension state to answer with.
	setCitySuspendedForTest(t, fs.cityPath, true)
	if age := cacheAgeOf(t, newTestCityHandler(t, fs), fs, "/status"); age != 0 {
		t.Errorf("GET /status on a suspended city X-GC-Cache-Age-S = %v, want 0: its body is built without reading any bead store", age)
	}
	setCitySuspendedForTest(t, fs.cityPath, false)
	if age := cacheAgeOf(t, newTestCityHandler(t, fs), fs, "/status"); age != 45 {
		t.Errorf("GET /status on the resumed city X-GC-Cache-Age-S = %v, want 45, the city store's cache age", age)
	}
}

// /status reports no bead-cache age for a body built on a suspended city on
// every path that serves the body from its response cache: the time bucket,
// the TTL floor, and the stale-while-revalidate serve, whose age is then how
// long ago the body was built. A body built on the resumed city, which read
// its stores, reports the cache's age on the same paths.
func TestCachedStatusOfASuspendedCityReportsNoCacheAge(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))
	oldTTL, oldFloor := timeBucketResponseCacheTTL, statusResponseTTLFloor
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})

	for _, tc := range []struct {
		name          string
		bucket, floor time.Duration
		// builtAgo backdates the cached body before the second GET. It stays
		// under the cache's 45 s age, so the stale serve, which reports the
		// greater of the two, shows which one it took.
		builtAgo     time.Duration
		suspendedAge float64
	}{
		{name: "time bucket", bucket: time.Hour},
		{name: "ttl floor", bucket: time.Nanosecond, floor: time.Hour},
		{name: "stale while revalidating", bucket: time.Nanosecond, builtAgo: 10 * time.Second, suspendedAge: 10},
	} {
		for _, suspended := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/suspended=%v", tc.name, suspended), func(t *testing.T) {
				timeBucketResponseCacheTTL, statusResponseTTLFloor = tc.bucket, tc.floor
				state := newFakeState(t)
				state.cityBeadStore = stubLivenessReporter{Store: beads.NewMemStore(), live: true, lastFresh: now.Add(-45 * time.Second)}
				setCitySuspendedForTest(t, state.cityPath, suspended)
				srv := New(state)
				h := newTestCityHandlerWith(t, state, srv)

				cacheAgeOf(t, h, state, "/status") // builds and caches the body
				backdateCachedResponse(t, srv, "status", tc.builtAgo)
				age := cacheAgeOf(t, h, state, "/status")
				srv.waitForBackground()

				want := 45.0
				if suspended {
					want = tc.suspendedAge
				}
				// The stale serve's age runs on the wall clock: compare whole seconds.
				if math.Floor(age) != want {
					t.Errorf("X-GC-Cache-Age-S = %v, want %v", age, want)
				}
			})
		}
	}
}
