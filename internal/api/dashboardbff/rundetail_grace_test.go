package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// TestUnknownRunGraceRefusesOversizedRunID proves an oversized runId is never
// tracked. The cap bounds ENTRIES, not bytes, so storing attacker-chosen ids
// verbatim would let a scanner spraying huge URIs at the unauthenticated /api
// plane pin ~cap x URI-length bytes per city. An oversized id must degrade to
// the immediate 404 (inGrace false) and leave the map untouched.
func TestUnknownRunGraceRefusesOversizedRunID(t *testing.T) {
	g, _ := newTestGrace(unknownRunGraceCap)

	if g.inGrace(strings.Repeat("x", unknownRunGraceMaxIDLen+1)) {
		t.Fatal("an oversized runId must not be graced")
	}
	g.mu.Lock()
	n := len(g.firstSeen)
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("map has %d entries after an oversized runId, want 0 (not tracked)", n)
	}
	// The bound is a security valve, not a functional limit: a runId at exactly
	// the bound is still tracked normally.
	if !g.inGrace(strings.Repeat("x", unknownRunGraceMaxIDLen)) {
		t.Fatal("a runId at exactly the length bound must still be graced")
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

// expectGracedWarming asserts rec carries the graced unknown-run 503 wire
// contract: HTTP 503, Retry-After: 5, and the runDetailErrorBody
// {"error":"run view is warming","reason":"unknown_run"} — distinguishable from
// the cold-replay warming 503, which stays a plain {error} body with no
// Retry-After header.
func expectGracedWarming(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	expectRunDetailStatus(t, rec, http.StatusServiceUnavailable)
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want %q", got, "5")
	}
	var body runDetailErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode graced 503 body: %v; body=%s", err, rec.Body.String())
	}
	if body.Error != "run view is warming" || body.Reason != "unknown_run" {
		t.Fatalf("graced 503 body = %+v, want error=%q reason=%q", body, "run view is warming", "unknown_run")
	}
}

// TestRunDetailEndpointUnknownRunWarmingGrace drives the JSON detail endpoint
// through the whole grace lifecycle for a truly-unknown run: the graced 503
// (Retry-After + unknown_run reason) on the first request, still graced within
// the window, and the plain 404 restored once the window expires.
func TestRunDetailEndpointUnknownRunWarmingGrace(t *testing.T) {
	p, _, clock := graceTestPlane(t)

	expectGracedWarming(t, getRunDetailRaw(t, p, "alpha", "missing"))
	*clock = clock.Add(unknownRunWarmingGrace - time.Second)
	expectGracedWarming(t, getRunDetailRaw(t, p, "alpha", "missing"))
	*clock = clock.Add(2 * time.Second)
	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", "missing"), http.StatusNotFound)
}

// TestRunDetailEndpointOversizedRunIDGets404 drives the oversized-id refusal
// through the JSON endpoint: the very first request answers the plain 404 (no
// grace window ever starts) and the tracker's map stays empty.
func TestRunDetailEndpointOversizedRunIDGets404(t *testing.T) {
	p, dir, _ := graceTestPlane(t)
	tl := p.runTailers.ensure("alpha", cityEventsPath(dir))

	huge := strings.Repeat("z", unknownRunGraceMaxIDLen+1)
	expectRunDetailStatus(t, getRunDetailRaw(t, p, "alpha", huge), http.StatusNotFound)
	tl.unknownRuns.mu.Lock()
	n := len(tl.unknownRuns.firstSeen)
	tl.unknownRuns.mu.Unlock()
	if n != 0 {
		t.Fatalf("grace map has %d entries after an oversized runId request, want 0", n)
	}
}

// TestRunDetailWarmingDoesNotStartGraceClock pins the check ORDER inside
// writeRunDetailReadError: the cold-replay warming answer (!ready) must win
// over — and must not consume — the unknown-run grace window. A not-found
// request during warming gets the PLAIN warming 503 (no Retry-After, no
// reason) and must not start the grace clock; the window is measured from the
// first POST-warm request, so even after a whole grace duration elapses during
// warming, the first warm request is still graced. A mutant that consults the
// grace tracker before the ready check starts (and here expires) the window
// during warming and answers 404 after warm-up, failing this test.
func TestRunDetailWarmingDoesNotStartGraceClock(t *testing.T) {
	prevPoll := runTailPollInterval
	prevWait := runColdLoadWait
	runTailPollInterval = 20 * time.Millisecond
	runColdLoadWait = 20 * time.Millisecond
	t.Cleanup(func() {
		runTailPollInterval = prevPoll
		runColdLoadWait = prevWait
	})
	dir := t.TempDir()
	writeEventLog(t, filepath.Join(dir, ".gc", "events.jsonl"), runDetailRootEvent())

	// Build the plane WITHOUT Start: the tailer exists but its fold loop is not
	// running, so the projection stays in the warming (!ready) state.
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	tl := p.runTailers.ensure("alpha", cityEventsPath(dir))
	cur := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	clock := &cur
	tl.unknownRuns.now = func() time.Time { return *clock }

	rec := getRunDetailRaw(t, p, "alpha", "missing")
	expectRunDetailStatus(t, rec, http.StatusServiceUnavailable)
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("cold-replay warming 503 must not set Retry-After, got %q", got)
	}
	var body runDetailErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode warming 503 body: %v; body=%s", err, rec.Body.String())
	}
	if body.Reason != "" {
		t.Fatalf("cold-replay warming 503 must carry no reason, got %q", body.Reason)
	}
	tl.unknownRuns.mu.Lock()
	_, tracked := tl.unknownRuns.firstSeen["missing"]
	tl.unknownRuns.mu.Unlock()
	if tracked {
		t.Fatal("a warming-phase request must not start the unknown-run grace clock")
	}

	// The warming phase outlives an entire grace window...
	*clock = clock.Add(unknownRunWarmingGrace + time.Second)

	// ...then the projection warms. The first post-warm request must STILL be
	// graced — its window starts now, not during warming.
	p.Start(t.Context())
	t.Cleanup(p.Stop)
	select {
	case <-tl.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("tailer never finished its cold replay")
	}
	expectGracedWarming(t, getRunDetailRaw(t, p, "alpha", "missing"))
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
// SSE precheck: the graced 503 (Retry-After + unknown_run reason) inside the
// grace window, before any stream body — plain 404 after it expires.
func TestRunDetailStreamUnknownRunWarmingGrace(t *testing.T) {
	p, _, clock := graceTestPlane(t)

	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/missing/detail/stream", nil))
	expectGracedWarming(t, rec)

	*clock = clock.Add(unknownRunWarmingGrace + time.Second)
	rec = httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/missing/detail/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 after the grace window; body=%s", rec.Code, rec.Body.String())
	}
}
