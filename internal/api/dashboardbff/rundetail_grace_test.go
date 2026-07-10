package dashboardbff

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// graphRunRootEvent builds a graph.v2 run-root molecule for runID with the
// same scope metadata shape as runDetailRootEvent, so a test can append a
// SECOND run to a log that already carries run1.
func graphRunRootEvent(seq uint64, runID string) events.Event {
	const formula = "mol-adopt-pr-v2"
	return beadCreatedEvent(seq, beads.Bead{
		ID:        runID,
		Title:     formula,
		Status:    "open",
		Type:      "molecule",
		Ref:       formula,
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          formula,
			"gc.run_target":       "rig:demo",
			"gc.root_store_ref":   "rig:demo",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "demo",
		},
	})
}

// newTestGrace builds an unknownRunGrace with the production window, a
// test-chosen capacity, and a manually advanced clock. The returned *time.Time
// is the clock: tests move it forward directly (all access is single-goroutine).
func newTestGrace(capacity int) (*unknownRunGrace, *time.Time) {
	cur := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	g := &unknownRunGrace{
		window:    unknownRunWarmingGrace,
		capacity:  capacity,
		now:       func() time.Time { return cur },
		firstSeen: make(map[string]time.Time),
	}
	return g, &cur
}

// TestUnknownRunGraceWindow proves the grace window is measured from the FIRST
// request for a runId: in-grace within the window, expired at/after it, and an
// expired runId stays expired (repeat polls must not restart the window).
func TestUnknownRunGraceWindow(t *testing.T) {
	g, clock := newTestGrace(unknownRunGraceCap)

	if !g.inGrace("run-x") {
		t.Fatal("first request for an unknown run must be in grace")
	}
	*clock = clock.Add(unknownRunWarmingGrace - time.Second)
	if !g.inGrace("run-x") {
		t.Fatal("request within the window must still be in grace")
	}
	*clock = clock.Add(2 * time.Second)
	if g.inGrace("run-x") {
		t.Fatal("request past the window must not be in grace")
	}
	if g.inGrace("run-x") {
		t.Fatal("an expired runId must stay expired on repeat requests (no window restart)")
	}
}

// TestUnknownRunGraceForget proves a runId that becomes known is dropped from
// the first-seen map immediately (it must not linger until cap pruning).
func TestUnknownRunGraceForget(t *testing.T) {
	g, _ := newTestGrace(unknownRunGraceCap)

	if !g.inGrace("run-x") {
		t.Fatal("first request must be in grace")
	}
	g.forget("run-x")
	g.mu.Lock()
	_, lingering := g.firstSeen["run-x"]
	n := len(g.firstSeen)
	g.mu.Unlock()
	if lingering || n != 0 {
		t.Fatalf("forget left %d entries (run-x present=%v), want empty map", n, lingering)
	}
}

// TestUnknownRunGraceCapEviction proves the first-seen map is bounded: a full
// map of live windows refuses new entries (they degrade to the plain 404, no
// live window is evicted), and expired entries are pruned to make room.
func TestUnknownRunGraceCapEviction(t *testing.T) {
	g, clock := newTestGrace(2)

	if !g.inGrace("run-1") || !g.inGrace("run-2") {
		t.Fatal("first two unknown runs must be tracked and in grace")
	}
	if g.inGrace("run-3") {
		t.Fatal("a full map of live windows must refuse a new runId (degrade to 404)")
	}
	g.mu.Lock()
	n := len(g.firstSeen)
	g.mu.Unlock()
	if n != 2 {
		t.Fatalf("map has %d entries after refused insert, want 2 (cap)", n)
	}

	// Expire the tracked windows: the next new runId prunes them and is tracked.
	*clock = clock.Add(unknownRunWarmingGrace + time.Second)
	if !g.inGrace("run-3") {
		t.Fatal("after the live windows expire, a new runId must prune and be tracked")
	}
	g.mu.Lock()
	_, r1 := g.firstSeen["run-1"]
	_, r2 := g.firstSeen["run-2"]
	_, r3 := g.firstSeen["run-3"]
	n = len(g.firstSeen)
	g.mu.Unlock()
	if r1 || r2 || !r3 || n != 1 {
		t.Fatalf("map after prune = %d entries (run-1=%v run-2=%v run-3=%v), want only run-3", n, r1, r2, r3)
	}
}

// graceTestPlane starts a plane over one city whose log already carries the
// canonical run1 root, warms the tailer, and installs a manually advanced clock
// on its unknown-run grace tracker. Everything runs on the test goroutine
// (ServeHTTP is synchronous), so the plain *time.Time clock is race-free.
func graceTestPlane(t *testing.T) (*Plane, string, *time.Time) {
	t.Helper()
	prev := runTailPollInterval
	runTailPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { runTailPollInterval = prev })
	dir := t.TempDir()
	writeEventLog(t, filepath.Join(dir, ".gc", "events.jsonl"), runDetailRootEvent())

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	// Warm the tailer first (a summary read blocks on the cold replay), so an
	// unknown run below is judged against the WARM projection.
	_ = getRunSummary(t, p, "alpha")

	tl := p.runTailers.ensure("alpha", cityEventsPath(dir))
	cur := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	clock := &cur
	tl.unknownRuns.now = func() time.Time { return *clock }
	return p, dir, clock
}

// TestRunDetailEndpointUnknownRunWarmingGrace drives the JSON detail endpoint
// through the whole grace lifecycle for a truly-unknown run: 503 warming on the
// first request, still 503 within the window, and the plain 404 restored once
// the window expires.
func TestRunDetailEndpointUnknownRunWarmingGrace(t *testing.T) {
	p, _, clock := graceTestPlane(t)

	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", "missing"), http.StatusServiceUnavailable)
	*clock = clock.Add(unknownRunWarmingGrace - time.Second)
	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", "missing"), http.StatusServiceUnavailable)
	*clock = clock.Add(2 * time.Second)
	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", "missing"), http.StatusNotFound)
}

// TestRunDetailEndpointKnownRunBypassesGrace proves a run the warm projection
// knows serves 200 untouched by the grace tracker, and that a runId which was
// unknown (tracked) and then appears in a later fold is dropped from the
// first-seen map on its next successful read.
func TestRunDetailEndpointKnownRunBypassesGrace(t *testing.T) {
	p, dir, _ := graceTestPlane(t)
	tl := p.runTailers.ensure("alpha", cityEventsPath(dir))

	// Known run: plain 200, and no grace entry is ever recorded for it.
	resp := getRunDetail(t, p, "alpha", "run1")
	if resp.RunID != "run1" {
		t.Fatalf("runId = %q, want run1", resp.RunID)
	}
	tl.unknownRuns.mu.Lock()
	n := len(tl.unknownRuns.firstSeen)
	tl.unknownRuns.mu.Unlock()
	if n != 0 {
		t.Fatalf("grace map has %d entries after a known-run read, want 0", n)
	}

	// A run slung but not yet folded: tracked and graced...
	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", "run2"), http.StatusServiceUnavailable)
	// ...then its root event arrives (the cache-reconcile catches up).
	appendEvents(t, filepath.Join(dir, ".gc", "events.jsonl"), graphRunRootEvent(2, "run2"))

	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := getRunDetailRaw(t, p, "alpha", "run2")
		if rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run2 never became readable; last status=%d body=%s", rec.Code, rec.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	tl.unknownRuns.mu.Lock()
	_, lingering := tl.unknownRuns.firstSeen["run2"]
	tl.unknownRuns.mu.Unlock()
	if lingering {
		t.Fatal("run2 became known but still lingers in the grace map")
	}
}

// TestRunDetailEndpointNotRunViewUnaffectedByGrace proves the 422 not_run_view
// answer is untouched by the grace window: a v1/wisp run's FIRST request — the
// one an unknown run would get graced on — still returns the definitive 422.
func TestRunDetailEndpointNotRunViewUnaffectedByGrace(t *testing.T) {
	dir := t.TempDir()
	// A molecule run marker but NO gc.formula_contract=graph.v2 → not a run view.
	writeEventLog(t, filepath.Join(dir, ".gc", "events.jsonl"), beadCreatedEvent(1, beads.Bead{
		ID:        "v1run",
		Title:     "legacy v1 run",
		Status:    "open",
		Type:      "molecule",
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		Metadata:  map[string]string{"gc.kind": "run"},
	}))

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()
	_ = getRunSummary(t, p, "alpha")

	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", "v1run"), http.StatusUnprocessableEntity)
	// And it stays 422 on a repeat — never demoted to warming or 404.
	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", "v1run"), http.StatusUnprocessableEntity)
}

// TestRunDetailStreamUnknownRunWarmingGrace mirrors the GET lifecycle on the
// SSE precheck: 503 warming inside the grace window (before any stream body),
// plain 404 after it expires.
func TestRunDetailStreamUnknownRunWarmingGrace(t *testing.T) {
	p, _, clock := graceTestPlane(t)

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/missing/detail/stream", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 inside the grace window; body=%s", rec.Code, rec.Body.String())
	}

	*clock = clock.Add(unknownRunWarmingGrace + time.Second)
	rec = httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/missing/detail/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 after the grace window; body=%s", rec.Code, rec.Body.String())
	}
}
