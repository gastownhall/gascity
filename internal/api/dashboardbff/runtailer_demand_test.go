package dashboardbff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/testutil"
)

// demandPollInterval is the shortened tail poll every test in this file runs
// with, so a fold lands within a few milliseconds of an append.
const demandPollInterval = 15 * time.Millisecond

// laneCount reads the published lane count under the tailer's lock WITHOUT
// asking for a projection. Every assertion about the deferred state must read
// this way: awaitProjection (and so waitForLanes) would itself drive the build
// the test is proving does not happen on its own.
func laneCount(tl *cityRunTailer) int {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	return len(tl.summary.Lanes)
}

// watchDeferredFolds installs the production deferral seam and returns a channel
// that carries one token per deferred fold. The test then waits on the FACT that
// the tail deferred a projection — no polling, no elapsed-time wait.
func watchDeferredFolds(t *testing.T) <-chan struct{} {
	t.Helper()
	deferred := make(chan struct{}, 1)
	runTailerAfterDeferredFold = func() {
		select {
		case deferred <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { runTailerAfterDeferredFold = nil })
	return deferred
}

// awaitDeferredFold blocks until the tail has folded a change and chosen not to
// project it. The timeout is the hang detector, not a latency bound: the seam
// fires inside the fold that would otherwise have built.
func awaitDeferredFold(t *testing.T, deferred <-chan struct{}) {
	t.Helper()
	select {
	case <-deferred:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("tail never deferred a folded change")
	}
}

// startDemandTailer builds a one-run event log and returns its started tailer,
// already past the cold replay (so the cold build has published).
func startDemandTailer(t *testing.T) (*cityRunTailer, string, func()) {
	t.Helper()
	prev := runTailPollInterval
	runTailPollInterval = demandPollInterval

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	m := newRunTailerManager(Deps{})
	m.enable(ctx, &wg)
	tl := m.ensure("alpha", logPath)
	stop := func() {
		cancel()
		wg.Wait()
		runTailPollInterval = prev
	}

	select {
	case <-tl.readyCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		stop()
		t.Fatal("cold replay did not complete")
	}
	// readyCh closes only after the cold replay's build published, so the first
	// generation is on the record with no waiting of any kind.
	if got := laneCount(tl); got != 1 {
		stop()
		t.Fatalf("published lanes after cold replay = %d, want 1", got)
	}
	return tl, logPath, stop
}

// TestRunTailerDefersProjectionWithoutReaders is the CPU fix: with nobody
// looking, a newly folded bead event must NOT re-project the city's run lanes.
// The fold still advances (so the on-demand build stays incremental), but the
// expensive projection waits for a reader.
func TestRunTailerDefersProjectionWithoutReaders(t *testing.T) {
	deferred := watchDeferredFolds(t)
	tl, logPath, stop := startDemandTailer(t)
	defer stop()

	buildsAfterColdLoad := projectionBuildCount.Load()

	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	awaitDeferredFold(t, deferred)

	if got := projectionBuildCount.Load(); got != buildsAfterColdLoad {
		t.Errorf("projection builds = %d, want %d: an unwatched fold re-projected the city", got, buildsAfterColdLoad)
	}
	if got := laneCount(tl); got != 1 {
		t.Errorf("published lanes = %d, want 1 while the projection is deferred", got)
	}
	if !tl.pendingProjection.Load() {
		t.Error("deferred fold did not record the unprojected debt")
	}

	// A reader asks: the deferred fold is projected exactly once, on demand.
	tl.awaitProjection(context.Background())
	if got := laneCount(tl); got != 2 {
		t.Errorf("published lanes after awaitProjection = %d, want 2", got)
	}
	if got := projectionBuildCount.Load(); got != buildsAfterColdLoad+1 {
		t.Errorf("projection builds = %d, want %d (exactly one on-demand build)", got, buildsAfterColdLoad+1)
	}
	if tl.pendingProjection.Load() {
		t.Error("pendingProjection still set after the on-demand build")
	}
}

// TestAwaitProjectionIsNoOpWhenCurrent proves a reader on an unchanged fold pays
// nothing: no wake, no rebuild. This is what keeps a polling client cheap.
func TestAwaitProjectionIsNoOpWhenCurrent(t *testing.T) {
	tl, _, stop := startDemandTailer(t)
	defer stop()

	before := projectionBuildCount.Load()
	tl.awaitProjection(context.Background())
	tl.awaitProjection(context.Background())

	if got := projectionBuildCount.Load(); got != before {
		t.Errorf("projection builds = %d, want %d: a reader rebuilt an unchanged fold", got, before)
	}
}

// TestRunTailerProjectsForDetailStreamSubscribers proves push semantics survive:
// a live SSE subscriber IS the demand, so a fold projects and notifies with no
// request driving it. The subscriber's own wakeup is the signal.
func TestRunTailerProjectsForDetailStreamSubscribers(t *testing.T) {
	deferred := watchDeferredFolds(t)
	tl, logPath, stop := startDemandTailer(t)
	defer stop()

	sub := tl.subscribe()
	defer tl.unsubscribe(sub)

	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	select {
	case <-sub.notify:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("subscriber was not notified: a watched fold did not publish")
	}

	if got := laneCount(tl); got != 2 {
		t.Errorf("published lanes = %d, want 2 pushed to the subscriber", got)
	}
	select {
	case <-deferred:
		t.Error("a watched fold deferred its projection instead of pushing")
	default:
	}
}

// TestRunSummaryEndpointProjectsDeferredFold drives the real endpoint: a request
// arriving after an unwatched fold serves the fresh lanes, so deferring the
// build costs freshness nowhere a client can observe it.
func TestRunSummaryEndpointProjectsDeferredFold(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = demandPollInterval
	deferred := watchDeferredFolds(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	if resp := getRunSummary(t, p, "alpha"); len(resp.Lanes) != 1 {
		t.Fatalf("lanes = %d, want 1 on the first request", len(resp.Lanes))
	}

	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	awaitDeferredFold(t, deferred)

	resp := getRunSummary(t, p, "alpha")
	if len(resp.Lanes) != 2 {
		t.Errorf("lanes = %d, want 2: the request did not project the deferred fold", len(resp.Lanes))
	}
}

// TestRunCensusProjectsDeferredFold covers the census reader on the same gate —
// every warm reader must project a deferred fold, not just the summary.
func TestRunCensusProjectsDeferredFold(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = demandPollInterval
	deferred := watchDeferredFolds(t)

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status = %d, want 200", rec.Code)
	}
	tailer, ok := p.cityRunTailer("alpha")
	if !ok {
		t.Fatal("tailer for alpha not found after a served request")
	}

	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	awaitDeferredFold(t, deferred)

	census, found := p.RunCensus(context.Background(), "alpha")
	if !found {
		t.Fatal("census for alpha not found")
	}
	if !census.Ready {
		t.Error("census not ready after a projected fold")
	}
	if tailer.pendingProjection.Load() {
		t.Error("census read left the folded change unprojected")
	}
}

// TestAwaitProjectionGivesUpOnTheBoundWhenNobodyServesTheWake covers the
// runProjectWait safety net — the one path where a reader waits on something
// that may never arrive. Plane.Stop cancels the tail loops, so a request that
// lands afterwards with a fold still unprojected has nobody to answer its wake.
// It must return on the bound with the previous generation rather than hold the
// request open, and it must not silently clear the debt it never paid.
func TestAwaitProjectionGivesUpOnTheBoundWhenNobodyServesTheWake(t *testing.T) {
	productionWait := runProjectWait
	runProjectWait = 25 * time.Millisecond
	t.Cleanup(func() { runProjectWait = productionWait })

	// No loop goroutine and no subscriber: exactly the shape left behind by
	// Plane.Stop. The published generation is built directly, as the cold replay
	// would have.
	tl := &cityRunTailer{name: "alpha", readyCh: make(chan struct{}), projectWake: make(chan struct{}, 1)}
	projector := runproj.NewProjector()
	projector.Apply([]events.Event{runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1")})
	tl.build(projector, nil, nil)

	// A fold landed that nothing projected, and nothing ever will.
	tl.pendingProjection.Store(true)
	buildsBefore := projectionBuildCount.Load()

	start := time.Now()
	tl.awaitProjection(context.Background())
	elapsed := time.Since(start)

	if elapsed < runProjectWait {
		t.Errorf("awaitProjection returned after %s, want it to wait out the %s bound", elapsed, runProjectWait)
	}
	// Threshold is the production default, not a multiple of the shortened wait:
	// it cannot flake on a loaded machine, and it still fails if the bound stops
	// being a var a test can shorten.
	if elapsed >= productionWait {
		t.Errorf("awaitProjection blocked %s, want the shortened %s bound honored (production default %s)", elapsed, runProjectWait, productionWait)
	}
	if got := laneCount(tl); got != 1 {
		t.Errorf("published lanes after the bound = %d, want 1: the previous generation must still be served", got)
	}
	if got := projectionBuildCount.Load(); got != buildsBefore {
		t.Errorf("projection builds = %d, want %d: nothing served the wake, so nothing should have built", got, buildsBefore)
	}
	if !tl.pendingProjection.Load() {
		t.Error("pendingProjection cleared without a build: the debt must survive for the next reader")
	}
}
