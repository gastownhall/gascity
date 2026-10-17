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
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
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
	if err := cacheLiveOr503(beadstest.Work(fakeLivenessStore{Store: mem})); err != nil {
		t.Fatalf("cacheLiveOr503(non-caching) = %v, want nil", err)
	}
}

func TestCacheLiveOr503_NilStorePasses(t *testing.T) {
	// A zero handle is treated as "no cache to gate" — the handler's own
	// nil-store guard (if any) is responsible for 503-on-no-store.
	if err := cacheLiveOr503(beads.WorkStore{}); err != nil {
		t.Fatalf("cacheLiveOr503(zero handle) = %v, want nil", err)
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
	if err := cacheLiveOr503(beadstest.Work(cache)); err != nil {
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
	err := cacheLiveOr503(beadstest.Work(cache))
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

// stubLivenessReporter is a fully controllable beads.CacheLivenessReporter for the
// cache-freshness conformance lane. It embeds a beads.Store so a class handle
// can wrap it for cacheAgeSeconds/cacheLiveOr503; only the two liveness
// methods those helpers actually call are implemented.
type stubLivenessReporter struct {
	beads.Store
	live      bool
	lastFresh time.Time
	// interval is the cadence Stats reports; 30 s when zero, as a primed
	// small cache reports.
	interval time.Duration
	paused   bool
	failures int
}

func (s stubLivenessReporter) IsLive() bool { return s.live }
func (s stubLivenessReporter) Stats() beads.CacheStats {
	interval := s.interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	return beads.CacheStats{
		LastFreshAt:              s.lastFresh,
		CurrentReconcileInterval: interval,
		ReconcilePaused:          s.paused,
		SyncFailures:             s.failures,
	}
}

func TestCacheAgeSeconds(t *testing.T) {
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	restore := SetLivenessClockForTest(&clock.Fake{Time: base})
	defer restore()

	ago := func(s int) time.Time { return base.Add(-time.Duration(s) * time.Second) }
	for _, tc := range []struct {
		name  string
		store beads.Store
		want  cacheFreshness
	}{
		{"nil-store", nil, cacheFreshness{}},
		{"non-caching", beads.NewMemStore(), cacheFreshness{}},
		{"unprimed-caching-store", beads.NewCachingStoreForTest(beads.NewMemStore(), nil), cacheFreshness{}},
		{"priming-never-fresh", stubLivenessReporter{live: false}, cacheFreshness{}},
		{
			"live-75s-30s-cadence",
			stubLivenessReporter{live: true, lastFresh: ago(75)},
			cacheFreshness{ageS: 75, refresh: CacheRefresh{IntervalS: 30, State: CacheRefreshScheduled}},
		},
		{
			"live-120s-cadence",
			stubLivenessReporter{live: true, lastFresh: ago(2), interval: 120 * time.Second},
			cacheFreshness{ageS: 2, refresh: CacheRefresh{IntervalS: 120, State: CacheRefreshScheduled}},
		},
		{
			"paused",
			stubLivenessReporter{live: true, lastFresh: ago(900), paused: true},
			cacheFreshness{ageS: 900, refresh: CacheRefresh{IntervalS: 30, State: CacheRefreshPaused}},
		},
		{
			"failing",
			stubLivenessReporter{live: true, lastFresh: ago(95), failures: 2},
			cacheFreshness{ageS: 95, refresh: CacheRefresh{IntervalS: 30, State: CacheRefreshFailing}},
		},
		{
			"paused-and-failing-is-paused",
			stubLivenessReporter{live: true, lastFresh: ago(95), failures: 2, paused: true},
			cacheFreshness{ageS: 95, refresh: CacheRefresh{IntervalS: 30, State: CacheRefreshPaused}},
		},
		{
			"clock-skew-negative-clamped",
			stubLivenessReporter{live: true, lastFresh: base.Add(5 * time.Second)},
			cacheFreshness{ageS: 0, refresh: CacheRefresh{IntervalS: 30, State: CacheRefreshScheduled}},
		},
		{
			"stats-only-policy-layer",
			statsOnlyStore{lastFresh: ago(45)},
			cacheFreshness{ageS: 45, refresh: CacheRefresh{IntervalS: 30, State: CacheRefreshScheduled}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cacheAgeSeconds(beadstest.Work(tc.store)); got != tc.want {
				t.Errorf("cacheAgeSeconds(%s) = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

// A primed real CachingStore behind a closed reconcile gate reports its
// re-scans paused, at the small-cache cadence, with the exact age.
func TestCacheAgeSecondsOfAGatedCachingStore(t *testing.T) {
	cache := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	beads.WithReconcileGate(func() bool { return false })(cache)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	lastFresh := cache.Stats().LastFreshAt
	if lastFresh.IsZero() {
		t.Fatal("expected non-zero LastFreshAt after Prime")
	}
	restore := SetLivenessClockForTest(&clock.Fake{Time: lastFresh.Add(12 * time.Second)})
	defer restore()
	want := cacheFreshness{ageS: 12, refresh: CacheRefresh{IntervalS: 30, State: CacheRefreshPaused}}
	if got := cacheAgeSeconds(beadstest.Work(cache)); got != want {
		t.Errorf("cacheAgeSeconds(gated primed cache) = %+v, want %+v", got, want)
	}
}

func TestCacheLiveOr503_StubStates(t *testing.T) {
	if err := cacheLiveOr503(beadstest.Work(stubLivenessReporter{live: true})); err != nil {
		t.Errorf("live stub = %v, want nil", err)
	}
	err := cacheLiveOr503(beadstest.Work(stubLivenessReporter{live: false}))
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
	return beads.CacheStats{LastFreshAt: s.lastFresh, CurrentReconcileInterval: 30 * time.Second}
}

func TestCacheAgeSecondsReadsAStoreThatReportsStatsWithoutLiveness(t *testing.T) {
	base := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	restore := SetLivenessClockForTest(&clock.Fake{Time: base})
	defer restore()

	store := statsOnlyStore{lastFresh: base.Add(-45 * time.Second)}
	if got := cacheAgeSeconds(beadstest.Work(store)).ageS; got != 45 {
		t.Errorf("cacheAgeSeconds(stats-only store).ageS = %v, want 45: the age must not depend on the store also reporting liveness", got)
	}
	if err := cacheLiveOr503(beadstest.Work(store)); err != nil {
		t.Errorf("cacheLiveOr503(stats-only store) = %v, want nil: a store without IsLive is never gated", err)
	}
}

// cacheHeadersOf GETs path from state's city through h and returns the
// response headers, failing unless it answers 200.
func cacheHeadersOf(t *testing.T, h http.Handler, state State, path string) http.Header {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, path), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Header()
}

// cacheAgeOf GETs path from state's city through h and returns the
// X-GC-Cache-Age-S it answers, 0 when the header is absent.
func cacheAgeOf(t *testing.T, h http.Handler, state State, path string) float64 {
	t.Helper()
	return cacheAgeIn(t, path, cacheHeadersOf(t, h, state, path))
}

// cacheAgeIn parses the X-GC-Cache-Age-S in hdr, 0 when absent.
func cacheAgeIn(t *testing.T, path string, hdr http.Header) float64 {
	t.Helper()
	raw := hdr.Get("X-GC-Cache-Age-S")
	if raw == "" {
		return 0
	}
	age, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("GET %s: X-GC-Cache-Age-S = %q: %v", path, raw, err)
	}
	return age
}

// Every output type that embeds CacheRefresh writes it beside its
// X-GC-Cache-Age-S header. Huma emits headers from an anonymous embedded
// struct; this pins it for each type, so a later refactor that drops the
// embedding is caught.
func TestCacheFreshnessHeadersOnEveryCachedReadType(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))

	fs := newFakeState(t)
	wait := subSecondWaitBead("w-1", now.Add(-time.Hour))
	fs.cityBeadStore = stubLivenessReporter{
		Store: beads.NewMemStoreFrom(1, []beads.Bead{wait}, nil),
		live:  true, lastFresh: now.Add(-900 * time.Second), paused: true,
	}
	fs.allOrders = []orders.Order{{Name: "nightly", Exec: "scripts/nightly.sh", Trigger: "cooldown", Interval: "1h"}}
	h := newTestCityHandler(t, fs)

	for _, path := range []string{
		"/beads", "/status", "/mail", "/mail/count", "/waits", "/wait/w-1",
		"/orders/history?scoped_name=nightly",
	} {
		hdr := cacheHeadersOf(t, h, fs, path)
		if got := hdr.Get("X-GC-Cache-Age-S"); got != "900" {
			t.Errorf("GET %s X-GC-Cache-Age-S = %q, want 900", path, got)
		}
		if got := hdr.Get("X-GC-Cache-Refresh-Interval-S"); got != "30" {
			t.Errorf("GET %s X-GC-Cache-Refresh-Interval-S = %q, want 30", path, got)
		}
		if got := hdr.Get("X-GC-Cache-Refresh"); got != "paused" {
			t.Errorf("GET %s X-GC-Cache-Refresh = %q, want paused", path, got)
		}
	}

	rigs := cacheHeadersOf(t, h, fs, "/rigs")
	if got := rigs.Get("X-GC-Cache-Refresh-Interval-S"); got != "0" {
		t.Errorf("GET /rigs X-GC-Cache-Refresh-Interval-S = %q, want 0: its body is not read from the bead cache", got)
	}
	if got, ok := rigs["X-Gc-Cache-Refresh"]; ok {
		t.Errorf("GET /rigs X-GC-Cache-Refresh = %q, want it omitted", got)
	}
}

// backdateSavedStatus moves the build time of the /status body srv saved d
// into the past.
func backdateSavedStatus(t *testing.T, srv *Server, d time.Duration) {
	t.Helper()
	srv.responseCacheMu.Lock()
	defer srv.responseCacheMu.Unlock()
	entry, ok := srv.responseCacheEntries["status"]
	if !ok {
		t.Fatal("no /status response saved")
	}
	entry.storedAt = entry.storedAt.Add(-d)
	srv.responseCacheEntries["status"] = entry
}

// setCitySuspendedForTest writes the city's runtime suspension state as gc
// suspend and gc resume do.
func setCitySuspendedForTest(t *testing.T, cityPath string, suspended bool) {
	t.Helper()
	if err := suspensionstate.SetCitySuspended(fsys.OSFS{}, cityPath, &suspended); err != nil {
		t.Fatal(err)
	}
}

// GET /rigs reports no cache age, however old the city store's cache is. Its
// body comes from config, the suspension state and runtime probes, not from the
// bead cache, so its age says nothing about it, and a CLI that saw it would
// print its cache-age footer over output that never came from the
// cache. Neither does GET /status on a suspended city, whose body is built
// without reading any bead store. GET /beads, which is read from the cache, is
// the control: it reports the same store's age, as GET /status does once the
// city is resumed.
func TestReadsNotServedFromTheBeadCacheReportNoCacheAge(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))

	fs := newFakeState(t)
	fs.cityBeadStore = stubLivenessReporter{Store: beads.NewMemStore(), live: true, lastFresh: now.Add(-45 * time.Second)}
	h := newTestCityHandler(t, fs)

	if age := cacheAgeOf(t, h, fs, "/beads"); age != 45 {
		t.Fatalf("precondition: GET /beads X-GC-Cache-Age-S = %v, want 45, the city store's cache age", age)
	}
	for _, path := range []string{"/rigs"} {
		hdr := cacheHeadersOf(t, h, fs, path)
		if age := cacheAgeIn(t, path, hdr); age != 0 {
			t.Errorf("GET %s X-GC-Cache-Age-S = %v, want 0: its body is not read from the bead cache", path, age)
		}
		if got := hdr.Values("X-GC-Cache-Refresh"); len(got) != 0 {
			t.Errorf("GET %s X-GC-Cache-Refresh = %q, want it omitted", path, got)
		}
	}

	// Each /status read goes through a new handler, whose server has no /status
	// body cached from the other suspension state to answer with.
	setCitySuspendedForTest(t, fs.cityPath, true)
	suspended := cacheHeadersOf(t, newTestCityHandler(t, fs), fs, "/status")
	if age := cacheAgeIn(t, "/status", suspended); age != 0 {
		t.Errorf("GET /status on a suspended city X-GC-Cache-Age-S = %v, want 0: its body is built without reading any bead store", age)
	}
	if got := suspended.Values("X-GC-Cache-Refresh"); len(got) != 0 {
		t.Errorf("GET /status on a suspended city X-GC-Cache-Refresh = %q, want it omitted", got)
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
		// suspendedRefresh is the X-GC-Cache-Refresh a suspended city's body
		// is served with; the resumed city's is always scheduled.
		suspendedRefresh string
	}{
		{name: "time bucket", bucket: time.Hour},
		{name: "ttl floor", bucket: time.Nanosecond, floor: time.Hour},
		{name: "stale while revalidating", bucket: time.Nanosecond, builtAgo: 10 * time.Second, suspendedAge: 10, suspendedRefresh: "rebuilding"},
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
				backdateSavedStatus(t, srv, tc.builtAgo)
				hdr := cacheHeadersOf(t, h, state, "/status")
				srv.waitForBackground()
				age := cacheAgeIn(t, "/status", hdr)

				want, wantRefresh := 45.0, "scheduled"
				if suspended {
					want, wantRefresh = tc.suspendedAge, tc.suspendedRefresh
				}
				// The stale serve's age runs on the wall clock: compare whole seconds.
				if math.Floor(age) != want {
					t.Errorf("X-GC-Cache-Age-S = %v, want %v", age, want)
				}
				if got := hdr.Get("X-GC-Cache-Refresh"); got != wantRefresh {
					t.Errorf("X-GC-Cache-Refresh = %q, want %q", got, wantRefresh)
				}
			})
		}
	}
}

// A stale-while-revalidate /status serve whose saved body is older than the
// city cache's own age reports the body's age and names the rebuild, so a
// client does not blame the cache's re-scans for the saved body's age.
func TestStaleStatusServeNamesTheRebuild(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))
	oldTTL, oldFloor := timeBucketResponseCacheTTL, statusResponseTTLFloor
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})
	timeBucketResponseCacheTTL, statusResponseTTLFloor = time.Nanosecond, 0

	state := newFakeState(t)
	state.cityBeadStore = stubLivenessReporter{Store: beads.NewMemStore(), live: true, lastFresh: now.Add(-5 * time.Second)}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	cacheAgeOf(t, h, state, "/status") // builds and saves the body
	backdateSavedStatus(t, srv, 140*time.Second)
	hdr := cacheHeadersOf(t, h, state, "/status")
	srv.waitForBackground()

	// The saved body's age runs on the wall clock: compare whole seconds.
	if age := cacheAgeIn(t, "/status", hdr); math.Floor(age) != 140 {
		t.Errorf("X-GC-Cache-Age-S = %v, want 140, the saved body's age", age)
	}
	if got := hdr.Get("X-GC-Cache-Refresh"); got != "rebuilding" {
		t.Errorf("X-GC-Cache-Refresh = %q, want rebuilding", got)
	}
	if got := hdr.Get("X-GC-Cache-Refresh-Interval-S"); got != "30" {
		t.Errorf("X-GC-Cache-Refresh-Interval-S = %q, want 30, the city cache's cadence", got)
	}
}

// A ?fresh=true /status read is built for the request, so it reports the city
// cache's age and refresh state, never a saved body's age or rebuilding. Its
// build replaces the saved body, so a plain read afterwards gets that build,
// not the old body named as rebuilding (ga-v1vyh8.17).
func TestFreshStatusReadReportsTheCacheNotARebuild(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	t.Cleanup(SetLivenessClockForTest(&clock.Fake{Time: now}))
	oldTTL, oldFloor := timeBucketResponseCacheTTL, statusResponseTTLFloor
	t.Cleanup(func() {
		timeBucketResponseCacheTTL = oldTTL
		statusResponseTTLFloor = oldFloor
	})
	timeBucketResponseCacheTTL, statusResponseTTLFloor = time.Nanosecond, 0

	state := newFakeState(t)
	state.cityBeadStore = stubLivenessReporter{Store: beads.NewMemStore(), live: true, lastFresh: now.Add(-5 * time.Second)}
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	cacheAgeOf(t, h, state, "/status") // builds and saves the body
	backdateSavedStatus(t, srv, 140*time.Second)
	hdr := cacheHeadersOf(t, h, state, "/status?fresh=true")
	srv.waitForBackground()

	if age := cacheAgeIn(t, "/status?fresh=true", hdr); age != 5 {
		t.Errorf("X-GC-Cache-Age-S = %v, want 5, the city cache's age", age)
	}
	if got := hdr.Get("X-GC-Cache-Refresh"); got != "scheduled" {
		t.Errorf("X-GC-Cache-Refresh = %q, want scheduled", got)
	}
	if got := hdr.Get("X-GC-Cache-Refresh-Interval-S"); got != "30" {
		t.Errorf("X-GC-Cache-Refresh-Interval-S = %q, want 30", got)
	}
	if len(srv.statusBuilds) != 0 {
		t.Errorf("statusBuilds = %v, want empty: the fresh read's build has ended", srv.statusBuilds)
	}
	if _, age, ok := staleResponseAs[indexedResponse[StatusBody]](srv, "status"); !ok || age >= 140*time.Second {
		t.Errorf("saved /status entry age = %v (ok=%v), want under 140s: the fresh read's build replaces the saved body", age, ok)
	}

	plain := cacheHeadersOf(t, h, state, "/status")
	srv.waitForBackground()
	if age := cacheAgeIn(t, "/status", plain); age != 5 {
		t.Errorf("plain X-GC-Cache-Age-S = %v, want 5, the city cache's age: the saved body is the fresh read's build", age)
	}
	if got := plain.Get("X-GC-Cache-Refresh"); got != "scheduled" {
		t.Errorf("plain X-GC-Cache-Refresh = %q, want scheduled", got)
	}
}
