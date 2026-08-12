package dashboardbff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/testutil"
)

// enrichmentCacheTestServer stands up a fake supervisor that counts sessions and
// formula hits and can gate a request open on a barrier so a test can force
// concurrent callers to overlap inside one in-flight fetch. sessionsBody /
// formulaStatus are read once per request; formulaStatus 0 means "200 with the
// canonical body".
type enrichmentCacheTestServer struct {
	sessionsHits atomic.Int64
	formulaHits  atomic.Int64

	// gate, when non-nil, blocks every handler until it is closed, so a test can
	// hold N callers inside one in-flight fetch and prove single-flight.
	gate chan struct{}

	formulaStatus atomic.Int64 // 0 -> 200 canonical body; else the status to return
}

func (s *enrichmentCacheTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.gate != nil {
			<-s.gate
		}
		switch r.URL.Path {
		case "/v0/city/alpha/sessions":
			s.sessionsHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"id":"s1","template":"t","session_name":"alpha__worker-1","title":"W","alias":"worker-1","state":"active","created_at":"2026-06-01T10:00:00Z","last_active":"2026-06-01T11:00:00Z","attached":false,"running":true,"activity":"thinking","provider":"claude"}],"total":1}`))
		case "/v0/city/alpha/formulas/mol-adopt-pr-v2":
			s.formulaHits.Add(1)
			if code := s.formulaStatus.Load(); code != 0 {
				w.WriteHeader(int(code))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"mol-adopt-pr-v2","steps":[{"id":"preflight"},{"id":"apply-fixes"}],"preview":{"nodes":[{"id":"preflight"},{"id":"apply-fixes"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newEnrichmentManager(t *testing.T, baseURL string) *runTailerManager {
	t.Helper()
	m := newRunTailerManager(Deps{SupervisorBaseURL: baseURL})
	return m
}

func TestSingleFlightCacheDiscardMatching(t *testing.T) {
	cache := newSingleFlightCache[string, int]()
	calls := map[string]int{}
	get := func(key string) int {
		value, ok := cache.get(context.Background(), key, func(context.Context) (int, time.Duration, bool, bool) {
			calls[key]++
			return calls[key], time.Hour, true, true
		})
		if !ok {
			t.Fatalf("get(%q) unavailable", key)
		}
		return value
	}

	if got := get("alpha"); got != 1 {
		t.Fatalf("first alpha value = %d, want 1", got)
	}
	if got := get("beta"); got != 1 {
		t.Fatalf("first beta value = %d, want 1", got)
	}
	cache.discardMatching(func(key string) bool { return key == "alpha" })
	if got := get("alpha"); got != 2 {
		t.Fatalf("invalidated alpha value = %d, want recomputed 2", got)
	}
	if got := get("beta"); got != 1 {
		t.Fatalf("unmatched beta value = %d, want cached 1", got)
	}
}

func TestRunTailerManagerRebindDiscardsInFlightEnrichment(t *testing.T) {
	manager := newRunTailerManager(Deps{})
	manager.ensure("alpha", "/city/first/.gc/events.jsonl")

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		_, _ = manager.sessionsCache.get(context.Background(), "alpha", func(context.Context) (cachedSessions, time.Duration, bool, bool) {
			close(started)
			<-release
			return cachedSessions{items: []runproj.DashboardSession{{ID: "old"}}}, time.Hour, true, true
		})
	}()
	select {
	case <-started:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("old enrichment compute did not start")
	}

	manager.ensure("alpha", "/city/rebound/.gc/events.jsonl")
	unblock()
	select {
	case <-oldDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("old enrichment compute did not finish")
	}

	newCalls := 0
	got, ok := manager.sessionsCache.get(context.Background(), "alpha", func(context.Context) (cachedSessions, time.Duration, bool, bool) {
		newCalls++
		return cachedSessions{items: []runproj.DashboardSession{{ID: "new"}}}, time.Hour, true, true
	})
	if !ok || len(got.items) != 1 || got.items[0].ID != "new" {
		t.Fatalf("post-rebind enrichment = %+v, available=%v; want freshly computed new value", got.items, ok)
	}
	if newCalls != 1 {
		t.Fatalf("post-rebind compute calls = %d, want 1; old in-flight value was retained", newCalls)
	}
}

// TestSingleFlightCacheRecoversAfterComputePanic proves a panic inside compute
// does not permanently wedge the key. The dashboardbff plane runs under
// withRecovery, so a compute panic is caught and the process keeps serving; the
// deferred release in get() must still clear inflight and close the in-flight
// channel so a later caller re-elects and succeeds instead of blocking forever
// on an orphaned channel.
func TestSingleFlightCacheRecoversAfterComputePanic(t *testing.T) {
	c := newSingleFlightCache[string, cachedSessions]()

	// First caller: compute panics. Recover it here, mimicking withRecovery. The
	// panic must PROPAGATE out of get (not be swallowed), while the deferred
	// release still clears the key.
	func() {
		defer func() { _ = recover() }()
		c.get(context.Background(), "alpha", func(context.Context) (cachedSessions, time.Duration, bool, bool) {
			panic("boom")
		})
		t.Fatal("expected the panicking compute to propagate out of get")
	}()

	// A subsequent caller for the SAME key must make progress, not deadlock on an
	// orphaned inflight channel.
	done := make(chan struct{})
	var ok bool
	go func() {
		defer close(done)
		_, ok = c.get(context.Background(), "alpha", func(context.Context) (cachedSessions, time.Duration, bool, bool) {
			return cachedSessions{items: []runproj.DashboardSession{}}, sessionsCacheTTL, true, true
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("post-panic get deadlocked on an orphaned inflight channel")
	}
	if !ok {
		t.Fatal("post-panic get: want available=true after a clean re-election, got false")
	}
}

// TestSingleFlightCacheElectorCancelDoesNotCancelSharedCompute proves the elected
// single-flight compute is decoupled from the electing caller's request context:
// if the elector's ctx is canceled while a joiner is still waiting on the shared
// flight, the shared compute must NOT be canceled, and the still-live joiner must
// receive the compute's real (successful) result instead of a canceled/degraded
// one. Before the fix the elector ran compute on its own request ctx, so an
// elector whose client disconnected mid-fetch canceled the shared upstream request
// and handed every joined caller a failed result rather than their own enrichment.
func TestSingleFlightCacheElectorCancelDoesNotCancelSharedCompute(t *testing.T) {
	c := newSingleFlightCache[string, int]()

	var (
		startOnce     sync.Once
		started       = make(chan struct{}) // closed when the elected compute begins
		release       = make(chan struct{}) // closed by the test to let compute finish
		computeCalls  atomic.Int64          // must stay 1: the joiner joins, never re-elects
		computeCtxErr error                 // compute's own ctx error, sampled after release
	)
	compute := func(cctx context.Context) (int, time.Duration, bool, bool) {
		computeCalls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		// Sample the compute context AFTER the elector's ctx was canceled. With the
		// fix this stays nil (detached); the pre-fix code would see context.Canceled
		// here, and a real upstream fetch on that ctx would abort.
		computeCtxErr = cctx.Err()
		if cctx.Err() != nil {
			// Mirror a real upstream fetch aborting on a canceled ctx: report failure.
			return 0, 0, false, false
		}
		return 42, time.Minute, true, true
	}

	electorCtx, cancelElector := context.WithCancel(context.Background())
	joinerCtx := context.Background() // the joiner stays live throughout

	var (
		electorVal, joinerVal int
		electorOK, joinerOK   bool
	)
	electorDone := make(chan struct{})
	go func() {
		defer close(electorDone)
		electorVal, electorOK = c.get(electorCtx, "k", compute)
	}()

	<-started // the elector was elected and is inside compute

	joinerDone := make(chan struct{})
	go func() {
		defer close(joinerDone)
		joinerVal, joinerOK = c.get(joinerCtx, "k", compute)
	}()

	// Let the joiner reach the in-flight join before we cancel the elector.
	time.Sleep(50 * time.Millisecond)

	// The electing caller's client disconnects: cancel its request ctx while the
	// joiner is still waiting on the shared flight.
	cancelElector()
	time.Sleep(20 * time.Millisecond) // give cancellation a chance to (wrongly) propagate

	close(release) // let the shared compute finish

	select {
	case <-joinerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("joiner never returned; the shared flight wedged")
	}
	<-electorDone

	if got := computeCalls.Load(); got != 1 {
		t.Fatalf("compute ran %d times, want 1 (the joiner must join the flight, not re-elect)", got)
	}
	if computeCtxErr != nil {
		t.Fatalf("shared compute ctx was canceled (%v); the elected compute must be decoupled from the electing caller's request", computeCtxErr)
	}
	if !joinerOK || joinerVal != 42 {
		t.Fatalf("joiner result = (%d,%v), want (42,true): a still-live joiner must get the shared compute's real result, not the canceled elector's degrade", joinerVal, joinerOK)
	}
	if !electorOK || electorVal != 42 {
		t.Fatalf("elector result = (%d,%v), want (42,true): the elected compute completes under its detached ctx", electorVal, electorOK)
	}
}

// TestSingleFlightCachePreCanceledColdCallerDoesNotElect proves a caller whose
// request context is already canceled before a cold-miss get does NOT elect the
// single-flight compute: it degrades to unavailable instead. Before the fix an
// already-canceled request could still become the elector, and because the
// elected compute runs under context.WithoutCancel(ctx) the detached fetch would
// then run a full upstream loopback (bounded only by the fetch/backstop timeout)
// on behalf of a caller that was already gone.
func TestSingleFlightCachePreCanceledColdCallerDoesNotElect(t *testing.T) {
	c := newSingleFlightCache[string, int]()

	var computeCalls atomic.Int64
	compute := func(context.Context) (int, time.Duration, bool, bool) {
		computeCalls.Add(1)
		return 42, time.Minute, true, true
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before get is even called

	v, ok := c.get(ctx, "k", compute)
	if got := computeCalls.Load(); got != 0 {
		t.Fatalf("compute ran %d times, want 0 (a pre-canceled caller must not elect a detached flight)", got)
	}
	if ok || v != 0 {
		t.Fatalf("pre-canceled cold get = (%d,%v), want (0,false): degrade to unavailable, not a detached compute", v, ok)
	}
}

// TestSingleFlightCachePreCanceledCallerServesLastGoodOnExpiredKey proves a caller
// whose context is already canceled at an EXPIRED key (past its TTL, so the
// fresh-hit path no longer applies) still does not elect a refetch: it degrades to
// the serveable last-good instead of driving a detached upstream compute for a
// caller that is already gone. Before the fix the pre-canceled caller re-elected
// and refetched, so compute ran a second time.
func TestSingleFlightCachePreCanceledCallerServesLastGoodOnExpiredKey(t *testing.T) {
	c := newSingleFlightCache[string, int]()

	var computeCalls atomic.Int64
	compute := func(context.Context) (int, time.Duration, bool, bool) {
		computeCalls.Add(1)
		return 7, 20 * time.Millisecond, true, true // staleServeable positive last-good, short TTL
	}

	// Seed a staleServeable positive last-good on a live ctx, then let it expire so
	// the next get can no longer take the fresh-hit path.
	if v, ok := c.get(context.Background(), "k", compute); !ok || v != 7 {
		t.Fatalf("seed get = (%d,%v), want (7,true)", v, ok)
	}
	time.Sleep(40 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v, ok := c.get(ctx, "k", compute)
	if got := computeCalls.Load(); got != 1 {
		t.Fatalf("compute ran %d times, want 1 (a pre-canceled caller must not elect a refetch on the expired key)", got)
	}
	if !ok || v != 7 {
		t.Fatalf("pre-canceled expired get = (%d,%v), want (7,true): serve the staleServeable last-good instead of a detached refetch", v, ok)
	}
}

// TestSessionsCacheServesWithinTTL proves two reads inside the TTL hit the
// supervisor exactly once (the second is served from cache).
func TestSessionsCacheServesWithinTTL(t *testing.T) {
	srv := &enrichmentCacheTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	s1, ok1 := m.fetchSessions(ctx, "alpha")
	s2, ok2 := m.fetchSessions(ctx, "alpha")
	if !ok1 || !ok2 {
		t.Fatalf("both reads must be available: %v %v", ok1, ok2)
	}
	if len(s1) != 1 || len(s2) != 1 {
		t.Fatalf("both reads must carry the one session: %d %d", len(s1), len(s2))
	}
	if got := srv.sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions upstream hits = %d, want 1 (second read is cached)", got)
	}
}

// TestSessionsCacheRefetchesAfterTTL proves a read past the TTL triggers a fresh
// upstream fetch.
func TestSessionsCacheRefetchesAfterTTL(t *testing.T) {
	defer func(prev time.Duration) { sessionsCacheTTL = prev }(sessionsCacheTTL)
	sessionsCacheTTL = 20 * time.Millisecond

	srv := &enrichmentCacheTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	if _, ok := m.fetchSessions(ctx, "alpha"); !ok {
		t.Fatal("first read must be available")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := m.fetchSessions(ctx, "alpha"); !ok {
		t.Fatal("second read must be available")
	}
	if got := srv.sessionsHits.Load(); got != 2 {
		t.Fatalf("sessions upstream hits = %d, want 2 (TTL expired between reads)", got)
	}
}

// TestSessionsCacheSingleFlight proves N concurrent cold-miss callers collapse
// to exactly one upstream fetch. The fake handler blocks on a gate so every
// caller is provably in-flight at once.
func TestSessionsCacheSingleFlight(t *testing.T) {
	srv := &enrichmentCacheTestServer{gate: make(chan struct{})}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	var available atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, ok := m.fetchSessions(ctx, "alpha"); ok {
				available.Add(1)
			}
		}()
	}
	// Give every goroutine time to reach the in-flight join before releasing.
	time.Sleep(50 * time.Millisecond)
	close(srv.gate)
	wg.Wait()

	if got := srv.sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions upstream hits = %d, want 1 (single-flight)", got)
	}
	if got := available.Load(); got != n {
		t.Fatalf("available callers = %d, want %d (all joiners see the shared result)", got, n)
	}
}

// TestSessionsCacheServesLastGoodOnError proves a fetch failure after a prior
// success serves the last-good value with available=true (degrade, don't blank).
func TestSessionsCacheServesLastGoodOnError(t *testing.T) {
	defer func(prev time.Duration) { sessionsCacheTTL = prev }(sessionsCacheTTL)
	sessionsCacheTTL = 20 * time.Millisecond

	var down atomic.Bool
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"s1","alias":"worker-1","state":"active"}],"total":1}`))
	}))
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	if s, ok := m.fetchSessions(ctx, "alpha"); !ok || len(s) != 1 {
		t.Fatalf("first read must succeed with one session: ok=%v n=%d", ok, len(s))
	}
	down.Store(true)
	time.Sleep(40 * time.Millisecond) // let the TTL lapse so the next read refetches

	s, ok := m.fetchSessions(ctx, "alpha")
	if !ok {
		t.Fatal("read after a failure must serve last-good (available=true)")
	}
	if len(s) != 1 {
		t.Fatalf("last-good sessions = %d, want 1", len(s))
	}
	if hits.Load() < 2 {
		t.Fatalf("upstream hits = %d, want >=2 (the failing refetch was attempted)", hits.Load())
	}
}

// TestSessionsCacheColdFailureDegrades proves a cold-miss failure with no
// last-good degrades EXACTLY as the uncached path: (nil, false).
func TestSessionsCacheColdFailureDegrades(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	s, ok := m.fetchSessions(context.Background(), "alpha")
	if ok || s != nil {
		t.Fatalf("cold failure must degrade to (nil,false); got n=%d ok=%v", len(s), ok)
	}
}

// TestSessionsCacheColdFailureSingleFlight proves a burst of concurrent cold-miss
// callers whose shared fetch FAILS collapses onto ONE upstream hit — every waiter
// returns that one failed flight's (nil,false) degrade instead of waking and
// re-electing itself to refetch serially. Regression for the single-flight
// burst-collapse contract on the negative path (the pre-fix code re-elected each
// waiter, producing one supervisor request per dashboard request).
func TestSessionsCacheColdFailureSingleFlight(t *testing.T) {
	var hits atomic.Int64
	gate := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-gate // hold every caller in-flight until released, proving overlap
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	var degraded atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if s, ok := m.fetchSessions(ctx, "alpha"); !ok && s == nil {
				degraded.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond) // let every caller reach the in-flight join
	close(gate)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("sessions upstream hits = %d, want 1 (a failed cold flight is shared, not re-elected per waiter)", got)
	}
	if got := degraded.Load(); got != n {
		t.Fatalf("degraded callers = %d, want %d (every waiter shares the one failed flight's (nil,false))", got, n)
	}
}

// TestFormulaCacheServesWithinTTL proves two formula reads inside the TTL hit the
// supervisor once and both resolve to the same available detail.
func TestFormulaCacheServesWithinTTL(t *testing.T) {
	srv := &enrichmentCacheTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	d1, f1, _, ok1 := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	d2, f2, _, ok2 := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	if !ok1 || !ok2 {
		t.Fatalf("both formula reads must succeed: %v %v (failures %q %q)", ok1, ok2, f1, f2)
	}
	if d1 == nil || d2 == nil || d1.Name != "mol-adopt-pr-v2" {
		t.Fatalf("both reads must carry the compiled detail: %+v %+v", d1, d2)
	}
	if got := srv.formulaHits.Load(); got != 1 {
		t.Fatalf("formula upstream hits = %d, want 1 (second read cached)", got)
	}
}

// TestFormulaCacheSingleFlight proves concurrent cold-miss formula callers
// collapse to one upstream fetch.
func TestFormulaCacheSingleFlight(t *testing.T) {
	srv := &enrichmentCacheTestServer{gate: make(chan struct{})}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	var okCount atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if _, _, _, ok := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); ok {
				okCount.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(srv.gate)
	wg.Wait()

	if got := srv.formulaHits.Load(); got != 1 {
		t.Fatalf("formula upstream hits = %d, want 1 (single-flight)", got)
	}
	if got := okCount.Load(); got != n {
		t.Fatalf("ok callers = %d, want %d", got, n)
	}
}

// TestFormulaCacheNotFoundReCheckedOnShortTTL proves a 404 is cached as
// FormulaDetailNotFound and re-checked on the SHORT not-found TTL — not the long
// success TTL — so a newly-added formula appears promptly.
func TestFormulaCacheNotFoundReCheckedOnShortTTL(t *testing.T) {
	defer func(prevOK, prevMiss time.Duration) {
		formulaCacheTTL = prevOK
		formulaNotFoundTTL = prevMiss
	}(formulaCacheTTL, formulaNotFoundTTL)
	// A long success TTL and a short not-found TTL: if the 404 were cached on the
	// success TTL it would never re-check inside the test window.
	formulaCacheTTL = 10 * time.Second
	formulaNotFoundTTL = 20 * time.Millisecond

	srv := &enrichmentCacheTestServer{}
	srv.formulaStatus.Store(http.StatusNotFound)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	// First read: 404 -> not_found, cached.
	_, failure, _, ok := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	if ok {
		t.Fatal("a 404 formula fetch must not report ok")
	}
	if failure != runproj.FormulaDetailNotFound {
		t.Fatalf("failure = %q, want not_found", failure)
	}
	// Second read inside the short TTL: served from the cached not-found entry, no
	// new upstream hit.
	if _, f2, _, ok2 := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); ok2 || f2 != runproj.FormulaDetailNotFound {
		t.Fatalf("cached not-found read: ok=%v failure=%q, want not_found", ok2, f2)
	}
	if got := srv.formulaHits.Load(); got != 1 {
		t.Fatalf("formula hits = %d, want 1 (second read within short TTL is cached)", got)
	}

	// After the SHORT not-found TTL lapses, the formula becomes available upstream.
	time.Sleep(40 * time.Millisecond)
	srv.formulaStatus.Store(0) // 200 canonical body
	d, _, _, ok := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	if !ok || d == nil {
		t.Fatalf("after the short not-found TTL the newly-added formula must resolve: ok=%v d=%+v", ok, d)
	}
	if got := srv.formulaHits.Load(); got != 2 {
		t.Fatalf("formula hits = %d, want 2 (the not-found entry expired and refetched)", got)
	}
}

// TestFormulaCacheColdFailureDegrades proves a cold-miss upstream error (non-404)
// with no last-good degrades to (nil, upstream_error, false) — the uncached
// contract.
func TestFormulaCacheColdFailureDegrades(t *testing.T) {
	srv := &enrichmentCacheTestServer{}
	srv.formulaStatus.Store(http.StatusInternalServerError)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	d, failure, _, ok := m.fetchFormulaDetailVersioned(context.Background(), "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	if ok || d != nil {
		t.Fatalf("cold upstream error must degrade to (nil,...,false); got d=%+v ok=%v", d, ok)
	}
	if failure != runproj.FormulaDetailUpstreamError {
		t.Fatalf("failure = %q, want upstream_error", failure)
	}
}

// TestFormulaCacheColdFailureSingleFlight proves the same negative-path
// burst-collapse for the formula cache: concurrent cold-miss callers whose shared
// fetch returns a non-404 upstream error all resolve to (nil, upstream_error,
// false) from ONE upstream hit, not one re-elected refetch per waiter.
func TestFormulaCacheColdFailureSingleFlight(t *testing.T) {
	var hits atomic.Int64
	gate := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-gate
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	var upstreamErr atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if d, f, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); !ok && d == nil && f == runproj.FormulaDetailUpstreamError {
				upstreamErr.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("formula upstream hits = %d, want 1 (a failed cold flight is shared, not re-elected per waiter)", got)
	}
	if got := upstreamErr.Load(); got != n {
		t.Fatalf("upstream_error degrades = %d, want %d (every waiter shares the one failed flight)", got, n)
	}
}

// TestFormulaCacheExpiredNotFoundDoesNotMaskUpstreamError proves a cached 404 is
// NOT served stale once its short TTL lapses: after the not-found window a fresh
// upstream 500 must surface as FormulaDetailUpstreamError, not the stale
// not_found. A negative last-good would otherwise pin a genuinely-missing verdict
// over a live upstream failure, hiding the real operator diagnostic.
func TestFormulaCacheExpiredNotFoundDoesNotMaskUpstreamError(t *testing.T) {
	defer func(prev time.Duration) { formulaNotFoundTTL = prev }(formulaNotFoundTTL)
	formulaNotFoundTTL = 20 * time.Millisecond

	srv := &enrichmentCacheTestServer{}
	srv.formulaStatus.Store(http.StatusNotFound)
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	// First read: 404 -> cached not_found.
	if _, failure, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); ok || failure != runproj.FormulaDetailNotFound {
		t.Fatalf("first read: ok=%v failure=%q, want not_found", ok, failure)
	}

	// Let the short not-found TTL lapse, then the upstream starts erroring (500).
	time.Sleep(40 * time.Millisecond)
	srv.formulaStatus.Store(http.StatusInternalServerError)

	// The expired not_found MUST NOT be served stale to mask the live 500: the
	// honest reason is upstream_error.
	d, failure, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	if ok || d != nil {
		t.Fatalf("expired not_found + upstream 500 must degrade to (nil,...,false); got d=%+v ok=%v", d, ok)
	}
	if failure != runproj.FormulaDetailUpstreamError {
		t.Fatalf("failure = %q, want upstream_error (stale not_found must not mask the 500)", failure)
	}
	if got := srv.formulaHits.Load(); got != 2 {
		t.Fatalf("formula hits = %d, want 2 (the expired not_found refetched and hit the 500)", got)
	}
}

// TestFormulaCacheExpiredNotFoundConcurrent500SingleFlight proves the hardest
// negative-path case: after a cached not_found expires, a concurrent burst that
// now hits a 500 collapses onto ONE refetch, and every waiter sees the honest
// upstream_error degrade. It combines two contracts under concurrency — the
// expired not_found must not be served stale to mask the 500, and it must not be
// re-probed once per waiter.
func TestFormulaCacheExpiredNotFoundConcurrent500SingleFlight(t *testing.T) {
	defer func(prev time.Duration) { formulaNotFoundTTL = prev }(formulaNotFoundTTL)
	formulaNotFoundTTL = 20 * time.Millisecond

	var hits atomic.Int64
	var status atomic.Int64
	status.Store(http.StatusNotFound)
	var gateActive atomic.Bool
	gate := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if gateActive.Load() {
			<-gate // hold only the concurrent burst in-flight together
		}
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	// Seed a cached not_found (ungated), then let its short TTL lapse.
	if _, f, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); ok || f != runproj.FormulaDetailNotFound {
		t.Fatalf("seed read: ok=%v failure=%q, want not_found", ok, f)
	}
	time.Sleep(40 * time.Millisecond) // expire the not-found entry
	status.Store(http.StatusInternalServerError)
	gateActive.Store(true)

	const n = 8
	var wg sync.WaitGroup
	var upstreamErr atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if d, f, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); !ok && d == nil && f == runproj.FormulaDetailUpstreamError {
				upstreamErr.Add(1)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	// One seed hit + exactly one shared burst hit: the expired not_found refetch
	// collapses to a single 500 flight, not one probe per waiter.
	if got := hits.Load(); got != 2 {
		t.Fatalf("formula upstream hits = %d, want 2 (1 seed + 1 shared burst flight)", got)
	}
	if got := upstreamErr.Load(); got != n {
		t.Fatalf("upstream_error degrades = %d, want %d (expired not_found must not mask the shared 500)", got, n)
	}
}

// TestFormulaCacheKeyedByScope proves the cache is keyed by the full
// (name,target,scopeKind,scopeRef) tuple: two distinct scopes each get their own
// upstream fetch rather than colliding on the formula name.
func TestFormulaCacheKeyedByScope(t *testing.T) {
	var hits atomic.Int64
	seen := map[string]bool{}
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		mu.Lock()
		seen[r.URL.RawQuery] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"mol-adopt-pr-v2","steps":[],"preview":{"nodes":[]}}`))
	}))
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	if _, _, _, ok := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); !ok {
		t.Fatal("first scope must resolve")
	}
	if _, _, _, ok := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:other", "rig", "other"); !ok {
		t.Fatal("second scope must resolve")
	}
	// Same key as the first read -> cached, no new hit.
	if _, _, _, ok := m.fetchFormulaDetailVersioned(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); !ok {
		t.Fatal("repeat of the first scope must resolve")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("formula hits = %d, want 2 (two distinct scope keys, third read cached)", got)
	}
}
