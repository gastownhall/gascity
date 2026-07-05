package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
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
	case <-time.After(2 * time.Second):
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

// TestPlaneStartDoesNotBlockOnColdLoad proves Start stays non-blocking: a city
// whose event log is large enough that its cold replay takes hundreds of
// milliseconds must not delay Start, which only spawns the fold goroutine. We
// assert Start returns well under the cold-load duration.
func TestPlaneStartDoesNotBlockOnColdLoad(t *testing.T) {
	dir := t.TempDir()
	writeLargeRunLog(t, cityEventsPath(dir), 20000)

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"big": dir}}})

	start := time.Now()
	p.Start(t.Context())
	elapsed := time.Since(start)
	t.Cleanup(p.Stop)

	// The cold replay of 20k events takes hundreds of ms (measured ~290ms). Start
	// only spawns the fold goroutine, so it must return in a tiny fraction of that.
	// A generous 100ms ceiling still proves Start did not wait on the fold while
	// tolerating a slow CI box.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("Plane.Start took %v; it must not block on the cold replay", elapsed)
	}

	// And the fold still completes in the background — Start being fast did not
	// skip the warm-up.
	p.runTailers.mu.Lock()
	tl := p.runTailers.cities["big"]
	p.runTailers.mu.Unlock()
	if tl == nil {
		t.Fatal("big city was not eager-started")
	}
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

// writeLargeRunLog writes n run-molecule events to path so a cold replay is
// measurably slow (for the non-blocking-Start proof).
func writeLargeRunLog(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	defer f.Close() //nolint:errcheck
	var b strings.Builder
	for i := 0; i < n; i++ {
		bead := beads.Bead{
			ID:     "run1",
			Title:  "mol-adopt-pr-v2",
			Status: "open",
			Type:   "molecule",
			Metadata: map[string]string{
				"gc.formula_contract": "graph.v2",
				"gc.kind":             "run",
				"gc.formula":          "mol-adopt-pr-v2",
			},
		}
		payload, _ := json.Marshal(struct {
			Bead beads.Bead `json:"bead"`
		}{bead})
		line, _ := json.Marshal(events.Event{Seq: uint64(i + 1), Type: events.BeadCreated, Payload: payload})
		b.Write(line)
		b.WriteByte('\n')
	}
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatalf("write log: %v", err)
	}
}
