package dashboardbff

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/testutil"
)

type fakeResolver struct {
	paths map[string]string
	// cities, when non-nil, is what Cities returns (so a test can control the
	// eager-warm set and ordering independently of the CityPath map, or model a
	// resolver whose registry is empty at Start). When nil, Cities is derived
	// from paths so existing tests get eager-warming for free.
	cities []CityRef
}

func (f fakeResolver) CityPath(name string) (string, bool) {
	p, ok := f.paths[name]
	return p, ok
}

func (f fakeResolver) Cities() []CityRef {
	if f.cities != nil {
		return f.cities
	}
	refs := make([]CityRef, 0, len(f.paths))
	for name, path := range f.paths {
		refs = append(refs, CityRef{Name: name, Path: path})
	}
	return refs
}

// runMoleculeEvent builds a bead.created event for a run-molecule lane carrying
// the markers isRunGroup recognizes plus an active assignee for session joins.
func runMoleculeEvent(seq uint64, id, formula, assignee string) events.Event {
	b := beads.Bead{
		ID:        id,
		Title:     formula,
		Status:    "open",
		Type:      "molecule",
		Assignee:  assignee,
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          formula,
		},
	}
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{b})
	return events.Event{Seq: seq, Type: events.BeadCreated, Payload: payload}
}

func writeEventLog(t *testing.T, path string, evts ...events.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var b strings.Builder
	for _, e := range evts {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// appendEvents appends events to an existing log via a plain O_APPEND handle —
// the supervisor's own write path. That it succeeds while the tailer is running
// proves the tailer is a pure reader (never a second writer holding the file).
func appendEvents(t *testing.T, path string, evts ...events.Event) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	defer f.Close() //nolint:errcheck
	for _, e := range evts {
		line, _ := json.Marshal(e)
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
}

func waitForLanes(t *testing.T, tl *cityRunTailer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tl.mu.RLock()
		n := len(tl.summary.Lanes)
		tl.mu.RUnlock()
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tl.mu.RLock()
	n := len(tl.summary.Lanes)
	tl.mu.RUnlock()
	t.Fatalf("lane count = %d, want %d within deadline", n, want)
}

// TestRunTailerColdLoadAndLiveTail proves the tailer cold-replays the existing
// log, then picks up newly appended events on its byte-offset tail — and that an
// external writer can still append while the tail runs (no second-writer lock).
func TestRunTailerColdLoadAndLiveTail(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	m := newRunTailerManager(Deps{})
	m.enable(ctx, &wg)
	tl := m.ensure("alpha", logPath)

	select {
	case <-tl.readyCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cold replay did not complete")
	}
	waitForLanes(t, tl, 1)

	// Append a second run via the supervisor's own append path while the tail runs.
	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	waitForLanes(t, tl, 2)

	cancel()
	wg.Wait()
}

func TestRunTailerManagerRebindsChangedEventsPath(t *testing.T) {
	firstDir := t.TempDir()
	firstPath := filepath.Join(firstDir, ".gc", "events.jsonl")
	writeEventLog(t, firstPath, runMoleculeEvent(1, "run-first", "test-formula", ""))
	secondDir := t.TempDir()
	secondPath := filepath.Join(secondDir, ".gc", "events.jsonl")
	writeEventLog(t, secondPath, runMoleculeEvent(1, "run-second", "test-formula", ""))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	m := newRunTailerManager(Deps{})
	m.enable(ctx, &wg)
	first := m.ensure("alpha", firstPath)
	select {
	case <-first.readyCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("first cold replay did not complete")
	}
	waitForLanes(t, first, 1)

	replacement := m.ensure("alpha", secondPath)
	if replacement == first {
		t.Fatal("changed events path reused the old city tailer")
	}
	if got := m.ensure("alpha", secondPath); got != replacement {
		t.Fatal("unchanged replacement path did not reuse the new tailer")
	}
	select {
	case <-first.doneCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replaced path-bound tailer did not stop")
	}
	select {
	case <-replacement.readyCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement cold replay did not complete")
	}
	waitForLanes(t, replacement, 1)
	replacement.mu.RLock()
	hasSecond := lanePresent(replacement, "run-second")
	hasFirst := lanePresent(replacement, "run-first")
	ids := laneIDsOf(replacement.summary.Lanes)
	replacement.mu.RUnlock()
	if !hasSecond || hasFirst {
		t.Fatalf("replacement lanes = %v, want only run-second", ids)
	}
}

func TestRunTailerLogsColdLoadFailureOnce(t *testing.T) {
	previousLoad := readRunColdLoad
	readRunColdLoad = func(*runproj.Projector, string) error {
		return errors.New("cold disk unavailable")
	}
	t.Cleanup(func() { readRunColdLoad = previousLoad })

	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLog) })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	m := newRunTailerManager(Deps{})
	m.enable(ctx, &wg)
	tailer := m.ensure("alpha", filepath.Join(t.TempDir(), ".gc", "events.jsonl"))
	select {
	case <-tailer.readyCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		wg.Wait()
		t.Fatal("cold replay attempt did not complete")
	}

	cancel()
	wg.Wait()
	if got := strings.Count(logs.String(), "cold replay failed"); got != 1 {
		t.Fatalf("cold replay failure log count = %d, want 1; logs=%q", got, logs.String())
	}
	if !strings.Contains(logs.String(), "cold disk unavailable") {
		t.Fatalf("cold replay log omitted raw cause: %q", logs.String())
	}
}

// TestRunTailerPrimeDoesNotBlockLiveTail is the regression guard for the
// startup sessions-prime blocking live polling: the best-effort prime runs off
// the tail's poll goroutine, so a slow or hung /v0 sessions loopback read cannot
// delay folding events appended right after cold replay — the exact startup
// window the eager warm-up exists to cover. A /sessions handler that never
// responds stands in for the stalled loopback; the tail must still fold a
// post-ready append while that prime is parked.
func TestRunTailerPrimeDoesNotBlockLiveTail(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	// /sessions blocks until released, standing in for a slow or hung loopback
	// read. The post-cold-load prime issues exactly this request; if it ran inline
	// on the tail's poll goroutine, live folding would stall here for up to the
	// HTTP client timeout (runSessionsFetchTimeout, 10s) — far past this test's
	// deadlines.
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
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	m := newRunTailerManager(Deps{SupervisorBaseURL: supervisor.URL})
	m.enable(ctx, &wg)
	tl := m.ensure("alpha", logPath)

	select {
	case <-tl.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cold replay did not complete")
	}
	waitForLanes(t, tl, 1)

	// The prime fires right after readyCh closes. Wait until it is in-flight and
	// parked in /sessions, so the append below races a genuinely-stalled prime
	// rather than one that already returned.
	deadline := time.Now().Add(2 * time.Second)
	for sessionsHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions prime not in-flight after cold replay: hits=%d, want 1", got)
	}

	// Append a new run AFTER cold replay (post-ready), so only the live tail poll
	// can fold it (captureTailCursor took the offset before the replay). With the
	// prime moved OFF the poll goroutine, the tail folds it within a few poll
	// intervals even though the prime is still parked — release is not closed until
	// cleanup. Inline priming would stall the poll here and this waitForLanes would
	// time out.
	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	waitForLanes(t, tl, 2)

	// The fold above completed while the prime was still parked in /sessions,
	// proving the prime never gated live polling.
	if got := sessionsHits.Load(); got != 1 {
		t.Fatalf("sessions prime hits = %d, want exactly 1 in-flight parked prime", got)
	}

	unblock()
	cancel()
	wg.Wait()
}

// TestRunTailerRotationCatchUp is the regression guard for the rotation
// event-drop: events written to the active log in the poll window before a
// rotation live only in the archived file, so on rotation the live tail must
// catch up across archives instead of resetting its byte offset and reading only
// the fresh active file. It drives foldNext directly (no ticker) so the
// pre-rotation runs are provably archived before the tailer next folds — the
// exact window the previous size-shrink reset silently dropped.
func TestRunTailerRotationCatchUp(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	rec, err := events.NewFileRecorder(logPath, io.Discard)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	defer rec.Close() //nolint:errcheck

	// Seed one run and cold-load it, so the tail cursor sits past the seed just
	// like a warm tailer that has already folded the pre-rotation history.
	rec.Record(runMoleculeEvent(0, "run1", "mol-adopt-pr-v2", "worker-1"))

	tl := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	proj := runproj.NewProjector()
	st := captureTailCursor(logPath)
	if loadErr := proj.ColdLoad(logPath); loadErr != nil {
		t.Fatalf("cold load: %v", loadErr)
	}
	st.marks = tl.build(proj, nil, nil)

	// Append two more runs to the ACTIVE file, then rotate before the tailer
	// folds them: after the rename these live only in the archived .gz.
	rec.Record(runMoleculeEvent(0, "run2", "mol-design-review-v2", "worker-2"))
	rec.Record(runMoleculeEvent(0, "run3", "mol-bugflow-v1", "worker-3"))
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatalf("force rotate: %v", err)
	}
	rec.WaitForRotations() // the pre-rotation runs are now only in the .gz archive.

	// A fresh run lands in the new active file after the rotation.
	rec.Record(runMoleculeEvent(0, "run4", "mol-adopt-pr-v2", "worker-4"))

	// One fold must reconcile the archived pre-rotation runs AND the fresh
	// active-file run — no sequence gap, no stale lane.
	tl.foldNext(proj, st)

	got := map[string]bool{}
	for _, lane := range tl.summary.Lanes {
		got[lane.ID] = true
	}
	for _, want := range []string{"run1", "run2", "run3", "run4"} {
		if !got[want] {
			t.Errorf("lane %q missing after rotation; lanes=%v", want, laneIDsOf(tl.summary.Lanes))
		}
	}
	if len(tl.summary.Lanes) != 4 {
		t.Errorf("lane count = %d, want 4; lanes=%v", len(tl.summary.Lanes), laneIDsOf(tl.summary.Lanes))
	}
}

func laneIDsOf(lanes []runproj.RunLane) []string {
	ids := make([]string, 0, len(lanes))
	for _, lane := range lanes {
		ids = append(ids, lane.ID)
	}
	return ids
}

// lanePresent reports whether a run lane with the given id is in the tailer's
// published summary. Safe to call directly in these single-goroutine tests that
// drive foldNext by hand (no live loop mutates t.summary concurrently).
func lanePresent(tl *cityRunTailer, id string) bool {
	for _, lane := range tl.summary.Lanes {
		if lane.ID == id {
			return true
		}
	}
	return false
}

// TestRunTailerRotationCatchUpInFlightArchive is the regression guard for the
// async-compression window that TestRunTailerRotationCatchUp does not exercise
// (it waits for the gzip). After the recorder renames the active log to a plain
// events.jsonl.rotating-* file it gzips it in the BACKGROUND, so between the
// rename and the canonical .gz the just-rotated events live only in the rotating
// file — invisible to the .gz archive walker. A poll that folds during that
// window must still catch them, not advance the tail past them for good. The
// window is staged deterministically here: a real ForceRotate's gzip goroutine
// races the fold, so driving foldNext "without WaitForRotations" would flake
// (the gzip sometimes wins and the .gz path masks the bug). os.Rename preserves
// the pre-rotation inode on the rotating file, exactly as the recorder does, so
// foldNext detects the rotation by identity.
func TestRunTailerRotationCatchUpInFlightArchive(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A warm tailer that has already folded run1 (cursor past seq 1), like a
	// tailer mid-run when a rotation happens.
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))
	tl := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	proj := runproj.NewProjector()
	st := captureTailCursor(logPath)
	if loadErr := proj.ColdLoad(logPath); loadErr != nil {
		t.Fatalf("cold load: %v", loadErr)
	}
	st.marks = tl.build(proj, nil, nil)

	// run2, run3 land on the active log in the poll window before the rotation.
	appendEvents(t, logPath,
		runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"),
		runMoleculeEvent(3, "run3", "mol-bugflow-v1", "worker-3"),
	)

	// Rotation, staged as the recorder does it but with the gzip still pending:
	// rename the active log (seq 1-3) to a plain rotating-* file — no .gz yet —
	// then open a fresh active log. The rename carries the old inode to the
	// rotating file, so the fresh active log is a distinct identity.
	rotating := filepath.Join(filepath.Dir(logPath), "events.jsonl.rotating-20260601T120000Z-seq-1-3")
	if err := os.Rename(logPath, rotating); err != nil {
		t.Fatalf("rename to rotating: %v", err)
	}
	writeEventLog(t, logPath, runMoleculeEvent(4, "run4", "mol-adopt-pr-v2", "worker-4"))

	// One fold must reconcile the in-flight pre-rotation runs AND the fresh
	// active-file run — the drop happens only if the catch-up ignores the
	// rotating file.
	tl.foldNext(proj, st)

	got := map[string]bool{}
	for _, lane := range tl.summary.Lanes {
		got[lane.ID] = true
	}
	for _, want := range []string{"run1", "run2", "run3", "run4"} {
		if !got[want] {
			t.Errorf("lane %q missing after in-flight rotation; lanes=%v", want, laneIDsOf(tl.summary.Lanes))
		}
	}
	if len(tl.summary.Lanes) != 4 {
		t.Errorf("lane count = %d, want 4; lanes=%v", len(tl.summary.Lanes), laneIDsOf(tl.summary.Lanes))
	}
}

// TestRunTailerStartupCursorRotationRaceDoesNotSkip is the regression guard for
// the startup cursor split-stat race: the old capture read the byte offset and
// the active-file identity with two separate stats, so a rotation between them
// paired the OLD file's larger offset with the FRESH file's identity. The first
// foldNext then saw no identity change, ReadFrom seeked past the fresh file's EOF
// (reader.go returns the same offset when no bytes are available), and every
// fresh event below the stale offset was silently dropped until restart. It
// reproduces that exact corrupted cursor — a stale beyond-EOF offset on the
// current active identity — and proves one foldNext still folds the fresh event.
func TestRunTailerStartupCursorRotationRaceDoesNotSkip(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")

	// A warm tailer that already folded run1..run3 (cursor past seq 3) off a
	// larger pre-rotation active file.
	writeEventLog(t, logPath,
		runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"),
		runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"),
		runMoleculeEvent(3, "run3", "mol-bugflow-v1", "worker-3"),
	)
	tl := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	proj := runproj.NewProjector()
	if loadErr := proj.ColdLoad(logPath); loadErr != nil {
		t.Fatalf("cold load: %v", loadErr)
	}
	st := captureTailCursor(logPath)
	st.marks = tl.build(proj, nil, nil)
	staleOffset := st.offset // the pre-rotation (larger) active-file size

	// The fresh post-rotation active file is smaller and carries run4. Rewriting
	// in place keeps a stable identity — exactly the one the racy two-stat capture
	// would have paired with the OLD file's larger offset — so foldNext sees no
	// identity change and must instead recover from the stale beyond-EOF offset.
	writeEventLog(t, logPath, runMoleculeEvent(4, "run4", "mol-adopt-pr-v2", "worker-4"))
	freshInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat fresh active: %v", err)
	}
	if staleOffset <= freshInfo.Size() {
		t.Fatalf("precondition: stale offset %d must exceed fresh size %d", staleOffset, freshInfo.Size())
	}
	st.offset = staleOffset
	st.activeInfo = freshInfo

	tl.foldNext(proj, st)

	if !lanePresent(tl, "run4") {
		t.Errorf("run4 skipped: a fresh event below the stale startup offset was dropped; lanes=%v", laneIDsOf(tl.summary.Lanes))
	}
	for _, want := range []string{"run1", "run2", "run3"} {
		if !lanePresent(tl, want) {
			t.Errorf("pre-rotation lane %q lost; lanes=%v", want, laneIDsOf(tl.summary.Lanes))
		}
	}
}

func TestRunTailerRotationDuringActiveReadDoesNotCommitUnverifiedCursor(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "test-formula", ""))

	tailer := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	projector := runproj.NewProjector()
	if err := projector.ColdLoad(logPath); err != nil {
		t.Fatalf("cold load: %v", err)
	}
	state := captureTailCursor(logPath)
	state.marks = tailer.build(projector, nil, nil)
	oldOffset := state.offset
	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "test-formula", ""))

	previous := readTailEvents
	t.Cleanup(func() { readTailEvents = previous })
	rotated := false
	readTailEvents = func(path string, offset int64) ([]events.Event, int64, error) {
		evts, nextOffset, err := previous(path, offset)
		if err == nil && !rotated {
			rotated = true
			rotating := filepath.Join(filepath.Dir(path), "events.jsonl.rotating-20260601T120000Z-seq-1-2")
			if err := os.Rename(path, rotating); err != nil {
				t.Fatalf("rotate after active read: %v", err)
			}
			writeEventLog(t, path, runMoleculeEvent(3, "run3", "test-formula", ""))
		}
		return evts, nextOffset, err
	}

	tailer.foldNext(projector, state)
	if got := projector.LastSeq(); got != 1 {
		t.Fatalf("projector cursor = %d, want 1 until active read identity is verified", got)
	}
	if state.offset != oldOffset {
		t.Fatalf("byte cursor = %d, want preserved %d after unverified active read", state.offset, oldOffset)
	}
	if !tailer.summary.LanesPartial {
		t.Fatal("rotation during active read did not mark projection partial")
	}

	readTailEvents = previous
	tailer.foldNext(projector, state)
	for _, want := range []string{"run1", "run2", "run3"} {
		if !lanePresent(tailer, want) {
			t.Errorf("lane %q missing after verified rotation recovery; lanes=%v", want, laneIDsOf(tailer.summary.Lanes))
		}
	}
	if tailer.summary.LanesPartial {
		t.Fatal("verified rotation recovery did not clear recoverable incompleteness")
	}
}

// TestRunTailerRotationCatchUpErrorRetriesNextPoll is the regression guard for
// the rotation catch-up state-commit gap: on a detected rotation the tailer must
// catch up the just-rotated events (now only in the archive) BEFORE advancing its
// active identity and resetting its offset. The old code committed the fresh
// identity and reset the offset even when the catch-up read failed, so the next
// poll saw no rotation (SameFile) and the run2/run3 window was lost until restart.
// A transient catch-up error must instead leave the old identity in place so the
// next poll re-detects the rotation and recovers the whole window.
func TestRunTailerRotationCatchUpErrorRetriesNextPoll(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")

	// Warm tailer that folded run1 (cursor past seq 1).
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))
	tl := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	proj := runproj.NewProjector()
	if loadErr := proj.ColdLoad(logPath); loadErr != nil {
		t.Fatalf("cold load: %v", loadErr)
	}
	st := captureTailCursor(logPath)
	st.marks = tl.build(proj, nil, nil)
	preRotationInfo := st.activeInfo

	// run2, run3 land on the active log in the poll window, then a rotation moves
	// them to a plain rotating-* archive and opens a fresh active file (run4). The
	// rename carries the old inode to the rotating file, so the fresh active file
	// is a distinct identity foldNext detects as a rotation.
	appendEvents(t, logPath,
		runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"),
		runMoleculeEvent(3, "run3", "mol-bugflow-v1", "worker-3"),
	)
	rotating := filepath.Join(dir, ".gc", "events.jsonl.rotating-20260601T120000Z-seq-2-3")
	if err := os.Rename(logPath, rotating); err != nil {
		t.Fatalf("rename to rotating: %v", err)
	}
	writeEventLog(t, logPath, runMoleculeEvent(4, "run4", "mol-adopt-pr-v2", "worker-4"))

	// Fail the first catch-up read, then fall through to the real reader.
	defer func(prev func(string, events.Filter) ([]events.Event, error)) { readRotationCatchUp = prev }(readRotationCatchUp)
	realCatchUp := events.ReadFilteredWithInFlight
	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLog) })

	calls := 0
	readRotationCatchUp = func(path string, f events.Filter) ([]events.Event, error) {
		calls++
		if calls <= 2 {
			return nil, errors.New("transient catch-up read error")
		}
		return realCatchUp(path, f)
	}

	// First poll: catch-up errors. Nothing folds, and the tailer must not advance
	// its active identity or the next poll can no longer re-detect the rotation.
	// It must also publish the projection as incomplete until a cursor-preserving
	// retry proves that the failed rotation window was recovered.
	tl.foldNext(proj, st)
	if lanePresent(tl, "run2") || lanePresent(tl, "run3") || lanePresent(tl, "run4") {
		t.Fatalf("events folded despite a catch-up error; lanes=%v", laneIDsOf(tl.summary.Lanes))
	}
	if !os.SameFile(preRotationInfo, st.activeInfo) {
		t.Fatalf("active identity advanced on a catch-up error; the next poll can no longer re-detect the rotation")
	}
	if !tl.summary.LanesPartial {
		t.Fatal("rotation catch-up error did not mark the published projection partial")
	}

	// The active path can be briefly absent while a rotation is between rename
	// and recreation. ReadFrom treats ENOENT as an empty successful read, but that
	// must not clear the still-latched catch-up failure before the archived window
	// is recovered.
	gapPath := logPath + ".rotation-gap"
	if err := os.Rename(logPath, gapPath); err != nil {
		t.Fatalf("stage active-path rotation gap: %v", err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(gapPath); err == nil {
			_ = os.Rename(gapPath, logPath)
		}
	})
	tl.foldNext(proj, st)
	if !tl.summary.LanesPartial {
		t.Fatal("ENOENT rotation gap cleared an unresolved catch-up failure")
	}
	if err := os.Rename(gapPath, logPath); err != nil {
		t.Fatalf("restore active path after rotation gap: %v", err)
	}

	// A repeated poll in the same failed episode remains partial but does not
	// flood the log at the tailer's one-second production cadence.
	tl.foldNext(proj, st)
	if got := strings.Count(logs.String(), "rotation catch-up failed"); got != 1 {
		t.Fatalf("catch-up failure log count = %d, want 1 for one failure transition; logs=%q", got, logs.String())
	}

	// Third poll: catch-up succeeds and recovers the whole rotation window.
	tl.foldNext(proj, st)
	for _, want := range []string{"run1", "run2", "run3", "run4"} {
		if !lanePresent(tl, want) {
			t.Errorf("lane %q missing after catch-up retry; lanes=%v", want, laneIDsOf(tl.summary.Lanes))
		}
	}
	if tl.summary.LanesPartial {
		t.Fatal("successful cursor-preserving catch-up retry did not clear recoverable incompleteness")
	}
}

func TestRunTailerReadErrorPreservesCursorAndMarksProjectionIncomplete(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	tl := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	proj := runproj.NewProjector()
	if err := proj.ColdLoad(logPath); err != nil {
		t.Fatalf("cold load: %v", err)
	}
	st := captureTailCursor(logPath)
	st.marks = tl.build(proj, nil, nil)
	appendEvents(t, logPath, runMoleculeEvent(2, "run2", "mol-design-review-v2", "worker-2"))
	oldOffset := st.offset

	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLog) })

	previous := readTailEvents
	t.Cleanup(func() { readTailEvents = previous })
	failRead := true
	readTailEvents = func(path string, offset int64) ([]events.Event, int64, error) {
		if failRead {
			return nil, 0, errors.New("transient active-log read error")
		}
		return previous(path, offset)
	}
	tl.foldNext(proj, st)
	tl.foldNext(proj, st)

	if st.offset != oldOffset {
		t.Fatalf("offset advanced on read error: got %d, want %d", st.offset, oldOffset)
	}
	if !tl.summary.LanesPartial {
		t.Fatal("active-log read error did not mark the projection partial")
	}
	if got := strings.Count(logs.String(), "active-log tail failed"); got != 1 {
		t.Fatalf("active-tail failure log count = %d, want 1 for one failure transition; logs=%q", got, logs.String())
	}

	failRead = false
	gapPath := logPath + ".active-gap"
	if err := os.Rename(logPath, gapPath); err != nil {
		t.Fatalf("stage active-path gap: %v", err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(gapPath); err == nil {
			_ = os.Rename(gapPath, logPath)
		}
	})
	tl.foldNext(proj, st)
	if !tl.summary.LanesPartial {
		t.Fatal("ENOENT active-path gap cleared an unresolved tail-read failure")
	}
	if err := os.Rename(gapPath, logPath); err != nil {
		t.Fatalf("restore active path after gap: %v", err)
	}

	tl.foldNext(proj, st)
	if !lanePresent(tl, "run2") {
		t.Fatalf("retry from preserved cursor did not recover run2; lanes=%v", laneIDsOf(tl.summary.Lanes))
	}
	if tl.summary.LanesPartial {
		t.Fatal("successful active-log retry did not clear recoverable incompleteness")
	}

	appendEvents(t, logPath, runMoleculeEvent(3, "run3", "mol-bugflow-v1", "worker-3"))
	failRead = true
	tl.foldNext(proj, st)
	if got := strings.Count(logs.String(), "active-log tail failed"); got != 2 {
		t.Fatalf("active-tail failure log count after recovery = %d, want 2 transitions; logs=%q", got, logs.String())
	}
	failRead = false
	tl.foldNext(proj, st)
	if !lanePresent(tl, "run3") || tl.summary.LanesPartial {
		t.Fatalf("second retry did not recover a complete run3 projection; lanes=%v partial=%v", laneIDsOf(tl.summary.Lanes), tl.summary.LanesPartial)
	}
}

func TestRunTailerSuccessfulEmptyRetryClearsIncrementalFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "test-formula", ""))

	tailer := &cityRunTailer{name: "alpha", eventsPath: logPath, readyCh: make(chan struct{})}
	projector := runproj.NewProjector()
	if err := projector.ColdLoad(logPath); err != nil {
		t.Fatalf("cold load: %v", err)
	}
	state := captureTailCursor(logPath)
	state.marks = tailer.build(projector, nil, nil)

	previous := readTailEvents
	t.Cleanup(func() { readTailEvents = previous })
	readTailEvents = func(string, int64) ([]events.Event, int64, error) {
		return nil, 0, errors.New("transient empty-tail failure")
	}
	tailer.foldNext(projector, state)
	if !tailer.summary.LanesPartial {
		t.Fatal("read failure did not mark projection partial")
	}

	readTailEvents = previous
	tailer.foldNext(projector, state)
	if tailer.summary.LanesPartial {
		t.Fatal("successful retry with no new events did not clear recoverable incompleteness")
	}
}

// TestRunSummaryEndpointEnrichesFromSessions drives the full endpoint: the warm
// fold plus request-time session enrich resolves a lane's session to available
// health and an available census.
func TestRunSummaryEndpointEnrichesFromSessions(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	sessions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/alpha/sessions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"s1","template":"t","session_name":"alpha__worker-1","title":"W","alias":"worker-1","state":"active","created_at":"2026-06-01T10:00:00Z","last_active":"2026-06-01T11:00:00Z","attached":false,"running":true,"activity":"thinking","provider":"claude"}],"total":1}`))
	}))
	defer sessions.Close()

	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: sessions.URL,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	resp := getRunSummary(t, p, "alpha")
	if resp.TotalActive != 1 || len(resp.Lanes) != 1 {
		t.Fatalf("totalActive=%d lanes=%d, want 1/1", resp.TotalActive, len(resp.Lanes))
	}
	lane := resp.Lanes[0]
	if lane.Health.Status != "available" {
		t.Errorf("lane health = %q, want available", lane.Health.Status)
	}
	if lane.Health.Data.Session.Status != "resolved" {
		t.Errorf("session status = %q, want resolved", lane.Health.Data.Session.Status)
	}
	if resp.Census.Status != "available" {
		t.Errorf("census status = %q, want available", resp.Census.Status)
	}
}

// TestRunSummaryEndpointDegradesWithoutSessions proves a sessions outage degrades
// lane health to unavailable (counted unverifiable in the census) rather than
// failing the load.
func TestRunSummaryEndpointDegradesWithoutSessions(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runMoleculeEvent(1, "run1", "mol-adopt-pr-v2", "worker-1"))

	// No SupervisorBaseURL: the sessions read is unavailable.
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)
	defer p.Stop()

	resp := getRunSummary(t, p, "alpha")
	if len(resp.Lanes) != 1 {
		t.Fatalf("lanes = %d, want 1", len(resp.Lanes))
	}
	if resp.Lanes[0].Health.Status != "unavailable" {
		t.Errorf("lane health = %q, want unavailable on sessions outage", resp.Lanes[0].Health.Status)
	}
	if resp.Census.Status != "available" {
		t.Errorf("census status = %q, want available", resp.Census.Status)
	}
	if resp.Census.Data.TotalInFlight < 1 || resp.Census.Data.Unverifiable < 1 {
		t.Errorf("census = %+v, want >=1 in-flight and >=1 unverifiable", resp.Census.Data)
	}
}

// TestRunSummaryEndpointUnknownCity404s confirms an unresolvable city 404s.
func TestRunSummaryEndpointUnknownCity404s(t *testing.T) {
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{}}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/ghost/runs/summary", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown city", rec.Code)
	}
}

// runSummaryWire is the decoded endpoint body — a structural contract check that
// the wire carries the enriched RunSummary shape the SPA renderer reads.
type runSummaryWire struct {
	TotalActive int `json:"totalActive"`
	Lanes       []struct {
		ID     string `json:"id"`
		Health struct {
			Status string `json:"status"`
			Data   struct {
				PhaseConfidence string `json:"phaseConfidence"`
				Session         struct {
					Status string `json:"status"`
				} `json:"session"`
			} `json:"data"`
		} `json:"health"`
	} `json:"lanes"`
	Census struct {
		Status string `json:"status"`
		Data   struct {
			TotalInFlight int `json:"totalInFlight"`
			Unverifiable  int `json:"unverifiable"`
		} `json:"data"`
	} `json:"census"`
}

func getRunSummary(t *testing.T, p *Plane, city string) runSummaryWire { //nolint:unparam // city is fixed today but kept for parity with the other run helpers
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/"+city+"/runs/summary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp runSummaryWire
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}
