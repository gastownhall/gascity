package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runproj"
)

// seedRunLog writes a minimal one-run event log under dir/.gc/events.jsonl and
// returns dir (the city root the resolver reports).
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
	case <-time.After(hangBudget):
		t.Fatalf("cold replay for %q did not complete within deadline", tl.name)
	}
}

// TestPlaneStartDoesNotCreateTailers proves an idle supervisor does not start a
// cold event-history replay for every registered city at startup.
func TestPlaneStartDoesNotCreateTailers(t *testing.T) {
	alpha := seedRunLog(t, "alpha")
	beta := seedRunLog(t, "beta")

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": alpha, "beta": beta}}})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	p.runTailers.mu.Lock()
	got := len(p.runTailers.cities)
	p.runTailers.mu.Unlock()
	if got != 0 {
		t.Fatalf("Plane.Start created %d tailers without demand, want 0", got)
	}
}

// TestFirstRunSummaryRequestLazilyStartsTailer proves the first API demand
// creates exactly one city tailer and returns its folded projection.
func TestFirstRunSummaryRequestLazilyStartsTailer(t *testing.T) {
	dir := seedRunLog(t, "alpha")
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	if got := lenTailers(p); got != 0 {
		t.Fatalf("tailers before API demand = %d, want 0", got)
	}
	resp := getRunSummary(t, p, "alpha")
	if len(resp.Lanes) != 1 || resp.Lanes[0].ID != "run-alpha" {
		t.Fatalf("summary lanes = %+v, want one run-alpha lane", resp.Lanes)
	}
	if got := lenTailers(p); got != 1 {
		t.Fatalf("tailers after first API demand = %d, want 1", got)
	}
}

func lenTailers(p *Plane) int {
	p.runTailers.mu.Lock()
	defer p.runTailers.mu.Unlock()
	return len(p.runTailers.cities)
}

func TestPlaneStartNilResolverNoop(t *testing.T) {
	p := New(Deps{})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	if n := lenTailers(p); n != 0 {
		t.Fatalf("nil resolver started %d tailers, want 0", n)
	}
}

// TestDemandBeforeStartLaunchesWhenPlaneStarts proves a demand that arrives
// while the plane is being assembled is not stranded: Start must launch the
// already-created tailer's fold loop once the manager has its lifecycle
// context and waitgroup.
func TestDemandBeforeStartLaunchesWhenPlaneStarts(t *testing.T) {
	dir := seedRunLog(t, "alpha")
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	tl, ok := p.cityRunTailer("alpha")
	if !ok {
		t.Fatal("pre-Start demand did not resolve alpha")
	}
	if tl.started {
		t.Fatal("pre-Start demand unexpectedly launched the tailer")
	}

	p.Start(t.Context())
	t.Cleanup(p.Stop)
	waitReady(t, tl)
	if !tl.started {
		t.Fatal("tailer became ready without being marked started")
	}
}

// TestFirstDemandColdLoadTimeoutRetriesAfterReady is the dogfood regression
// for a slow real-history replay. The first demand must expose the documented
// 200/partial response while the replay is still blocked, then the same API
// must return the ready lane after the replay completes.
func TestFirstDemandColdLoadTimeoutRetriesAfterReady(t *testing.T) {
	previousWait := runColdLoadWait
	previousLoad := readRunColdLoad
	runColdLoadWait = 20 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLoad := func() { releaseOnce.Do(func() { close(release) }) }
	readRunColdLoad = func(proj *runproj.Projector, path string) error {
		close(started)
		<-release
		return previousLoad(proj, path)
	}
	t.Cleanup(func() {
		readRunColdLoad = previousLoad
		runColdLoadWait = previousWait
	})

	dir := seedRunLog(t, "alpha")
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	t.Cleanup(func() {
		releaseLoad()
		p.Stop()
	})
	if got := lenTailers(p); got != 0 {
		t.Fatalf("tailers before first demand = %d, want 0", got)
	}

	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/summary", nil))
		responses <- rec
	}()
	select {
	case <-started:
	case <-time.After(hangBudget):
		t.Fatal("first summary demand did not trigger cold load")
	}
	var first runSummaryWire
	select {
	case rec := <-responses:
		if rec.Code != http.StatusOK {
			t.Fatalf("first summary status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
			t.Fatalf("decode first summary: %v; body=%s", err, rec.Body.String())
		}
	case <-time.After(hangBudget):
		t.Fatal("first summary did not return the bounded warming response")
	}
	if !first.LanesPartial {
		t.Fatalf("first summary lanesPartial = false, want true while cold load is blocked; lanes=%+v", first.Lanes)
	}
	if len(first.Lanes) != 0 {
		t.Fatalf("first summary exposed %d lane(s) before ready, want none", len(first.Lanes))
	}

	releaseLoad()
	tl, ok := p.cityRunTailer("alpha")
	if !ok {
		t.Fatal("alpha resolver rejected the started tailer")
	}
	select {
	case <-tl.readyCh:
	case <-time.After(hangBudget):
		t.Fatal("cold load did not become ready after release")
	}
	second := getRunSummary(t, p, "alpha")
	if second.LanesPartial {
		t.Fatalf("retry summary lanesPartial = true, want false; lanes=%+v", second.Lanes)
	}
	if len(second.Lanes) != 1 || second.Lanes[0].ID != "run-alpha" {
		t.Fatalf("retry summary lanes = %+v, want one run-alpha lane", second.Lanes)
	}
}

// TestFirstDemandPrimesSessionsCache proves the tailer's best-effort sessions
// prime remains intact after startup becomes lazy: one first demand starts the
// fold, and a subsequent detail read uses the primed cache.
func TestFirstDemandPrimesSessionsCache(t *testing.T) {
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
	writeEventLog(t, cityEventsPath(dir), runDetailRootEvent(), runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"))
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}, SupervisorBaseURL: supervisor.URL})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	_ = getRunSummary(t, p, "alpha")
	p.runTailers.mu.Lock()
	tl := p.runTailers.cities["alpha"]
	p.runTailers.mu.Unlock()
	if tl == nil {
		t.Fatal("first API demand did not create alpha tailer")
	}
	waitReady(t, tl)

	deadline := time.Now().Add(2 * time.Second)
	for sessionsHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions upstream hits after first demand = %d, want exactly 1", got)
	}
	if _, _, err := tl.detail(t.Context(), "run1"); err != nil {
		t.Fatalf("detail after prime: %v", err)
	}
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions upstream hits after warm detail() = %d, want 1", got)
	}
}

// TestPlaneStopDoesNotBlockOnWedgedSessionsPrime guards the optional prime's
// shutdown behavior. A first demand starts a tailer whose detached sessions
// fetch is wedged; Stop must still return promptly.
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
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}, SupervisorBaseURL: supervisor.URL})
	p.Start(t.Context())

	tl, ok := p.cityRunTailer("alpha")
	if !ok {
		t.Fatal("alpha resolver rejected a registered city")
	}
	waitReady(t, tl)
	deadline := time.Now().Add(2 * time.Second)
	for sessionsHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions prime not in-flight: hits=%d, want 1", got)
	}

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Plane.Stop blocked on the wedged best-effort sessions prime")
	}
	unblock()
}
