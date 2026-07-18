package dashboardbff

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/testutil"
)

// seedRunLog writes a minimal one-run event log under dir/.gc/events.jsonl and
// returns dir (the city root the resolver reports). Used by the eager-warm tests
// so each city has a foldable run at Start.
func seedRunLog(t *testing.T, city string) string {
	t.Helper()
	dir := t.TempDir()
	writeEventLog(t, cityEventsPath(dir), runMoleculeEvent(1, "run-"+city, "mol-adopt-pr-v2", "worker-1"))
	return dir
}

// waitReady blocks until the tailer's cold replay completes, failing the test if
// it does not within the deadline.
func waitReady(t *testing.T, tl *cityRunTailer) {
	t.Helper()
	select {
	case <-tl.readyCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("cold replay for %q did not complete within deadline", tl.name)
	}
}

// TestPlaneStartEagerWarmsAllCities proves the headline: Plane.Start eager-starts
// the run-view fold for every registered city, so each tailer's cold replay
// completes without any request touching the plane.
func TestPlaneStartEagerWarmsAllCities(t *testing.T) {
	alpha := seedRunLog(t, "alpha")
	beta := seedRunLog(t, "beta")

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": alpha, "beta": beta}}})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	// No GET has been issued. The tailers must nonetheless exist and warm on their
	// own (the eager fold), proving Start started them rather than the lazy
	// first-request path.
	for _, name := range []string{"alpha", "beta"} {
		p.runTailers.mu.Lock()
		tl, ok := p.runTailers.cities[name]
		p.runTailers.mu.Unlock()
		if !ok {
			t.Fatalf("city %q was not eager-started at Plane.Start", name)
		}
		waitReady(t, tl)
	}
}

func TestPlaneStartEagerWarmsRegistryCityNames(t *testing.T) {
	paths := map[string]string{
		"alpha_beta": seedRunLog(t, "alpha_beta"),
		"alpha.beta": seedRunLog(t, "alpha.beta"),
	}
	p := New(Deps{Resolver: fakeResolver{paths: paths}})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	for name := range paths {
		p.runTailers.mu.Lock()
		tailer, ok := p.runTailers.cities[name]
		p.runTailers.mu.Unlock()
		if !ok {
			t.Fatalf("registered city %q was not eager-started", name)
		}
		waitReady(t, tailer)
	}
}

// TestPlaneStartEagerNilResolverNoop proves a nil resolver is a no-op: Start does
// not panic and starts no tailers.
func TestPlaneStartEagerNilResolverNoop(t *testing.T) {
	p := New(Deps{})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	p.runTailers.mu.Lock()
	n := len(p.runTailers.cities)
	p.runTailers.mu.Unlock()
	if n != 0 {
		t.Fatalf("nil resolver started %d tailers, want 0", n)
	}
}

// TestPlaneStartEagerEmptyCitiesNoop proves an empty registry is a no-op.
func TestPlaneStartEagerEmptyCitiesNoop(t *testing.T) {
	p := New(Deps{Resolver: fakeResolver{cities: []CityRef{}}})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	p.runTailers.mu.Lock()
	n := len(p.runTailers.cities)
	p.runTailers.mu.Unlock()
	if n != 0 {
		t.Fatalf("empty Cities() started %d tailers, want 0", n)
	}
}

// TestPlaneStartDoesNotBlockOnColdLoad proves Start stays non-blocking while a
// cold replay is deterministically held in flight.
func TestPlaneStartDoesNotBlockOnColdLoad(t *testing.T) {
	dir := t.TempDir()
	writeEventLog(t, cityEventsPath(dir), runMoleculeEvent(1, "run-one", "test-formula", ""))

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	previousLoad := readRunColdLoad
	readRunColdLoad = func(projector *runproj.Projector, path string) error {
		close(started)
		<-release
		return previousLoad(projector, path)
	}
	t.Cleanup(func() { readRunColdLoad = previousLoad })

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"big": dir}}})
	startReturned := make(chan struct{})
	go func() {
		p.Start(t.Context())
		close(startReturned)
	}()
	t.Cleanup(func() {
		unblock()
		p.Stop()
	})

	select {
	case <-started:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("background cold replay did not start")
	}
	select {
	case <-startReturned:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Plane.Start blocked on the held cold replay")
	}

	p.runTailers.mu.Lock()
	tl := p.runTailers.cities["big"]
	p.runTailers.mu.Unlock()
	if tl == nil {
		t.Fatal("big city was not eager-started")
	}

	select {
	case <-tl.readyCh:
		t.Fatal("Plane.Start returned only after the cold replay completed; it must not block on the fold")
	default:
	}

	unblock()
	select {
	case <-tl.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("background cold replay did not complete")
	}
}

// TestPlaneStartPrimesSessionsCache proves the secondary win: after a city's cold
// replay completes, the tailer best-effort primes the per-city sessions cache
// with exactly one upstream /sessions read — WITHOUT any detail() or summary GET.
// A subsequent detail() then serves fully warm (no extra sessions hit within the
// TTL).
func TestPlaneStartPrimesSessionsCache(t *testing.T) {
	var sessionsHits atomic.Int64
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sessions") {
			sessionsHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer supervisor.Close()

	dir := t.TempDir()
	writeEventLog(t, cityEventsPath(dir),
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	p.runTailers.mu.Lock()
	tl := p.runTailers.cities["alpha"]
	p.runTailers.mu.Unlock()
	if tl == nil {
		t.Fatal("alpha was not eager-started")
	}
	waitReady(t, tl)

	// The prime runs right after readyCh closes; give the loop goroutine a moment
	// to issue the one best-effort fetch.
	deadline := time.Now().Add(2 * time.Second)
	for sessionsHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions upstream hits after cold-load = %d, want exactly 1 (best-effort prime, no request issued)", got)
	}

	// A detail() within the sessions TTL serves the primed cache: still exactly one
	// upstream hit (the prime), no inline fetch.
	if _, _, err := tl.detail(t.Context(), "run1"); err != nil {
		t.Fatalf("detail after prime: %v", err)
	}
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions upstream hits after a warm detail() = %d, want 1 (cache served from the prime)", got)
	}
}

// TestPlaneStopDoesNotBlockOnWedgedSessionsPrime is the regression guard for the
// startup sessions-prime pinning shutdown: the best-effort prime elects a
// single-flight compute that detaches from the plane ctx (enrichment_cache.go)
// and is bounded only by the HTTP client timeout, so a prime that waited on that
// compute inside the plane waitgroup would keep Plane.Stop blocked for up to
// runSessionsFetchTimeout on a wedged /sessions read — even though the prime is
// optional. A /sessions handler that never responds stands in for the wedged
// loopback; Stop must return promptly and let the detached fetch drain on its own.
func TestPlaneStopDoesNotBlockOnWedgedSessionsPrime(t *testing.T) {
	var sessionsHits atomic.Int64
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sessions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sessionsHits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer supervisor.Close()

	dir := t.TempDir()
	writeEventLog(t, cityEventsPath(dir), runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())

	p.runTailers.mu.Lock()
	tl := p.runTailers.cities["alpha"]
	p.runTailers.mu.Unlock()
	if tl == nil {
		t.Fatal("alpha was not eager-started")
	}
	waitReady(t, tl)

	// Wait until the prime is genuinely in-flight and parked in the wedged
	// /sessions read, so Stop races a stalled prime rather than one that already
	// returned.
	deadline := time.Now().Add(2 * time.Second)
	for sessionsHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions prime not in-flight after cold replay: hits=%d, want 1", got)
	}

	// Stop must not wait on the wedged prime. The /sessions read is never released,
	// so a Stop that drained the prime inline would block for up to the 10s HTTP
	// client timeout; the child-goroutine race must let Stop return promptly while
	// the detached fetch drains on its own bounded deadline.
	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Plane.Stop blocked on the wedged best-effort sessions prime; shutdown must not wait on the optional detached fetch")
	}

	// Release the wedged handler now that the Stop assertion has held. Otherwise
	// the leaked child's /sessions read stays parked and the deferred
	// supervisor.Close() blocks for the full HTTP client timeout draining it,
	// adding ~10s of pure teardown plus an alarming httptest blocked-close warning.
	unblock()
}
