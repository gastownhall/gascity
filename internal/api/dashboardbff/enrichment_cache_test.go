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

// TestFormulaCacheServesWithinTTL proves two formula reads inside the TTL hit the
// supervisor once and both resolve to the same available detail.
func TestFormulaCacheServesWithinTTL(t *testing.T) {
	srv := &enrichmentCacheTestServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	m := newEnrichmentManager(t, ts.URL)
	ctx := context.Background()

	d1, f1, ok1 := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	d2, f2, ok2 := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
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
			if _, _, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); ok {
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
	_, failure, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	if ok {
		t.Fatal("a 404 formula fetch must not report ok")
	}
	if failure != runproj.FormulaDetailNotFound {
		t.Fatalf("failure = %q, want not_found", failure)
	}
	// Second read inside the short TTL: served from the cached not-found entry, no
	// new upstream hit.
	if _, f2, ok2 := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); ok2 || f2 != runproj.FormulaDetailNotFound {
		t.Fatalf("cached not-found read: ok=%v failure=%q, want not_found", ok2, f2)
	}
	if got := srv.formulaHits.Load(); got != 1 {
		t.Fatalf("formula hits = %d, want 1 (second read within short TTL is cached)", got)
	}

	// After the SHORT not-found TTL lapses, the formula becomes available upstream.
	time.Sleep(40 * time.Millisecond)
	srv.formulaStatus.Store(0) // 200 canonical body
	d, _, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
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
	d, failure, ok := m.fetchFormulaDetail(context.Background(), "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo")
	if ok || d != nil {
		t.Fatalf("cold upstream error must degrade to (nil,...,false); got d=%+v ok=%v", d, ok)
	}
	if failure != runproj.FormulaDetailUpstreamError {
		t.Fatalf("failure = %q, want upstream_error", failure)
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

	if _, _, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); !ok {
		t.Fatal("first scope must resolve")
	}
	if _, _, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:other", "rig", "other"); !ok {
		t.Fatal("second scope must resolve")
	}
	// Same key as the first read -> cached, no new hit.
	if _, _, ok := m.fetchFormulaDetail(ctx, "alpha", "mol-adopt-pr-v2", "rig:demo", "rig", "demo"); !ok {
		t.Fatal("repeat of the first scope must resolve")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("formula hits = %d, want 2 (two distinct scope keys, third read cached)", got)
	}
}
