package dashboardbff

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// warmRunTailer builds a plane over a temp event log for the "alpha" city,
// starts it, and blocks until the run's detail is servable (the cold replay has
// folded the root). It returns the plane and the city's tailer so a test can
// drive detail() directly.
func warmRunTailer(t *testing.T, evts ...events.Event) (*Plane, *cityRunTailer) {
	t.Helper()
	const city = "alpha"
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, evts...)

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{city: dir}}})
	p.Start(t.Context())
	t.Cleanup(p.Stop)

	tl, ok := p.cityRunTailer(city)
	if !ok {
		t.Fatalf("cityRunTailer(%q) not found", city)
	}
	select {
	case <-tl.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cold replay did not complete")
	}
	return p, tl
}

// TestRunDetailMemoBuildsOncePerGeneration proves two detail() calls at the same
// fold generation build exactly once (the second is served from the memo) and
// return byte-identical results.
func TestRunDetailMemoBuildsOncePerGeneration(t *testing.T) {
	_, tl := warmRunTailer(t,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	ctx := context.Background()

	before := detailBuildCount.Load()
	v1, ready1, err1 := tl.detail(ctx, "run1")
	if err1 != nil || !ready1 {
		t.Fatalf("first detail: err=%v ready=%v", err1, ready1)
	}
	v2, ready2, err2 := tl.detail(ctx, "run1")
	if err2 != nil || !ready2 {
		t.Fatalf("second detail: err=%v ready=%v", err2, ready2)
	}

	if builds := detailBuildCount.Load() - before; builds != 1 {
		t.Fatalf("builds = %d across two same-generation detail() calls, want 1", builds)
	}
	if !bytes.Equal(v1.bytes, v2.bytes) {
		t.Fatalf("memoized bytes differ across two same-generation calls:\n%s\nvs\n%s", v1.bytes, v2.bytes)
	}
	if len(v1.bytes) == 0 {
		t.Fatal("memoized bytes are empty")
	}
}

// TestRunDetailMemoRebuildsOnNewFoldGeneration proves appending a bead event
// (which advances lastSeq) yields a new memo key → a rebuild.
func TestRunDetailMemoRebuildsOnNewFoldGeneration(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()
	tl, _ := p.cityRunTailer("alpha")
	select {
	case <-tl.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cold replay did not complete")
	}
	ctx := context.Background()

	before := detailBuildCount.Load()
	if _, _, err := tl.detail(ctx, "run1"); err != nil {
		t.Fatalf("first detail: %v", err)
	}
	seqBefore := currentLastSeq(tl)

	// Append a new step event; the tail folds it and bumps lastSeq.
	appendEvents(t, logPath, runDetailStepEvent(3, "run1.2", "run1", "rebase-check", "open"))
	waitForLastSeqAbove(t, tl, seqBefore)

	if _, _, err := tl.detail(ctx, "run1"); err != nil {
		t.Fatalf("second detail after append: %v", err)
	}
	if builds := detailBuildCount.Load() - before; builds != 2 {
		t.Fatalf("builds = %d, want 2 (a new fold generation must rebuild)", builds)
	}
}

// TestRunDetailMemoRebuildsOnSessionsVersionBump proves a sessions-cache refresh
// (a bumped version) alone rebuilds the detail, even at the same fold
// generation. The supervisor sessions endpoint is stable, so only the version
// changes across the TTL boundary.
func TestRunDetailMemoRebuildsOnSessionsVersionBump(t *testing.T) {
	defer func(prev time.Duration) { sessionsCacheTTL = prev }(sessionsCacheTTL)
	sessionsCacheTTL = 20 * time.Millisecond

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
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"))
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: supervisor.URL,
	})
	p.Start(t.Context())
	defer p.Stop()
	tl, _ := p.cityRunTailer("alpha")
	select {
	case <-tl.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cold replay did not complete")
	}
	ctx := context.Background()

	before := detailBuildCount.Load()
	if _, _, err := tl.detail(ctx, "run1"); err != nil {
		t.Fatalf("first detail: %v", err)
	}
	// Let the sessions cache TTL lapse so the next detail() refetches and bumps
	// the sessions version, changing the memo key even though the fold is
	// unchanged.
	time.Sleep(40 * time.Millisecond)
	if _, _, err := tl.detail(ctx, "run1"); err != nil {
		t.Fatalf("second detail: %v", err)
	}
	if builds := detailBuildCount.Load() - before; builds != 2 {
		t.Fatalf("builds = %d, want 2 (a sessions-version bump must rebuild)", builds)
	}
}

// TestRunDetailServedBytesEqualFreshMarshal proves the served HTTP body equals a
// fresh json.Marshal(detail) of the memoized DTO plus the single trailing
// newline the JSON encoder emits — i.e. writeJSONBytes is byte-identical to the
// old writeJSON(w, detail) path.
func TestRunDetailServedBytesEqualFreshMarshal(t *testing.T) {
	p, tl := warmRunTailer(t,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)

	// The memoized value carries both the DTO and its bytes.
	value, ready, err := tl.detail(context.Background(), "run1")
	if err != nil || !ready {
		t.Fatalf("detail: err=%v ready=%v", err, ready)
	}
	fresh, err := json.Marshal(value.detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	if !bytes.Equal(value.bytes, fresh) {
		t.Fatalf("memoized bytes != fresh json.Marshal(detail):\n%s\nvs\n%s", value.bytes, fresh)
	}

	// The served HTTP body is the memoized bytes + one trailing newline (the
	// encoder's), matching the pre-memo writeJSON path byte-for-byte.
	rec := getRunDetailRaw(t, p, "alpha", "run1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	want := append(append([]byte{}, fresh...), '\n')
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("served body != json.Marshal(detail)+\"\\n\":\n%q\nvs\n%q", rec.Body.Bytes(), want)
	}
}

// TestRunDetailMemoConcurrentSingleBuild proves N concurrent detail() calls for
// the same run at the same generation build exactly once (single-flight) and all
// return byte-identical results. Run under -race, it also proves the memo is
// race-clean.
func TestRunDetailMemoConcurrentSingleBuild(t *testing.T) {
	_, tl := warmRunTailer(t,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	ctx := context.Background()

	before := detailBuildCount.Load()
	const n = 12
	var wg sync.WaitGroup
	results := make([][]byte, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			v, _, err := tl.detail(ctx, "run1")
			results[i], errs[i] = v.bytes, err
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("concurrent detail[%d]: %v", i, errs[i])
		}
	}
	if builds := detailBuildCount.Load() - before; builds != 1 {
		t.Fatalf("builds = %d across %d concurrent same-generation calls, want 1 (single-flight)", builds, n)
	}
	for i := 1; i < n; i++ {
		if !bytes.Equal(results[0], results[i]) {
			t.Fatalf("concurrent result[%d] differs from result[0]", i)
		}
	}
}

// TestRunDetailMemoEvictsLRU proves the memo is bounded: once more distinct
// generations than the cap are inserted, the oldest is evicted (a re-request of
// it rebuilds). Uses a tiny cap so a few generations force eviction.
func TestRunDetailMemoEvictsLRU(t *testing.T) {
	defer func(prev int) { runDetailMemoCap = prev }(runDetailMemoCap)
	runDetailMemoCap = 2
	m := newRunDetailMemo()

	build := func(seq uint64) func() (runDetailMemoValue, error) {
		return func() (runDetailMemoValue, error) {
			detailBuildCount.Add(1)
			return runDetailMemoValue{bytes: []byte{byte(seq)}}, nil
		}
	}
	key := func(seq uint64) runDetailMemoKey { return runDetailMemoKey{runID: "run1", lastSeq: seq} }

	before := detailBuildCount.Load()
	// Fill: generations 1 and 2 (cap=2).
	if _, err := m.getOrBuild(key(1), build(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.getOrBuild(key(2), build(2)); err != nil {
		t.Fatal(err)
	}
	// Generation 3 evicts the least-recently-used (generation 1).
	if _, err := m.getOrBuild(key(3), build(3)); err != nil {
		t.Fatal(err)
	}
	// Generation 2 is still cached (no rebuild).
	if _, err := m.getOrBuild(key(2), build(2)); err != nil {
		t.Fatal(err)
	}
	// Generation 1 was evicted → rebuild.
	if _, err := m.getOrBuild(key(1), build(1)); err != nil {
		t.Fatal(err)
	}
	// Builds: gen1, gen2, gen3, (gen2 hit), gen1-rebuild = 4.
	if builds := detailBuildCount.Load() - before; builds != 4 {
		t.Fatalf("builds = %d, want 4 (gen2 cached, evicted gen1 rebuilt)", builds)
	}
}

// TestRunDetailMemoRecoversAfterBuildPanic proves a panic inside build() does
// not poison the memo with a zero-value entry. The dashboardbff plane runs under
// withRecovery, so a build panic is caught and the process keeps serving; the
// deferred cleanup in getOrBuild must store NOTHING (completed stays false),
// clear the in-flight handshake, and let the next caller re-elect and build a
// real value — never read cached empty bytes. Mirrors
// TestSingleFlightCacheRecoversAfterComputePanic for the LRU memo.
func TestRunDetailMemoRecoversAfterBuildPanic(t *testing.T) {
	m := newRunDetailMemo()
	key := runDetailMemoKey{runID: "run1", lastSeq: 1}

	// First caller: build panics. Recover it here, mimicking withRecovery. The
	// panic MUST propagate out of getOrBuild (not be swallowed) while the deferred
	// cleanup clears the key and stores nothing.
	func() {
		defer func() { _ = recover() }()
		_, _ = m.getOrBuild(key, func() (runDetailMemoValue, error) {
			panic("boom")
		})
		t.Fatal("expected the panicking build to propagate out of getOrBuild")
	}()

	// A subsequent caller for the SAME key must re-elect (not deadlock on an
	// orphaned inflight channel) and must build a fresh value rather than read a
	// cached zero-value entry a poisoning store would have left.
	done := make(chan struct{})
	var (
		got runDetailMemoValue
		err error
	)
	go func() {
		defer close(done)
		got, err = m.getOrBuild(key, func() (runDetailMemoValue, error) {
			return runDetailMemoValue{bytes: []byte("real")}, nil
		})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("post-panic getOrBuild deadlocked on an orphaned inflight channel")
	}
	if err != nil {
		t.Fatalf("post-panic getOrBuild: %v", err)
	}
	if string(got.bytes) != "real" {
		t.Fatalf("post-panic getOrBuild returned bytes %q, want the freshly built %q "+
			"(a poisoned zero-value entry would be empty)", got.bytes, "real")
	}
}

// TestRunDetailSameGenerationSkipsSnapshotFold proves two detail() calls at the
// same fold generation fold the run's snapshot exactly once — the second is
// served from the snapshot cache with no re-scan. Before the snapshot cache the
// detail memo skipped the build+marshal on a hit, but detail() still re-ran
// SnapshotForRun on every request, so the hot repeat GET kept scanning the run.
func TestRunDetailSameGenerationSkipsSnapshotFold(t *testing.T) {
	_, tl := warmRunTailer(t,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	ctx := context.Background()

	before := snapshotFoldCount.Load()
	if _, ready, err := tl.detail(ctx, "run1"); err != nil || !ready {
		t.Fatalf("first detail: err=%v ready=%v", err, ready)
	}
	if _, ready, err := tl.detail(ctx, "run1"); err != nil || !ready {
		t.Fatalf("second detail: err=%v ready=%v", err, ready)
	}
	if folds := snapshotFoldCount.Load() - before; folds != 1 {
		t.Fatalf("snapshot folds = %d across two same-generation detail() calls, want 1 "+
			"(the second must hit the snapshot cache)", folds)
	}
}

// TestRunDetailNewGenerationRefolds proves appending a bead event (which advances
// lastSeq) invalidates the snapshot cache, so the next detail() re-folds — the
// snapshot cache must not serve a stale fold across generations.
func TestRunDetailNewGenerationRefolds(t *testing.T) {
	defer func(prev time.Duration) { runTailPollInterval = prev }(runTailPollInterval)
	runTailPollInterval = 15 * time.Millisecond

	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runDetailRootEvent(),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()
	tl, _ := p.cityRunTailer("alpha")
	select {
	case <-tl.readyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("cold replay did not complete")
	}
	ctx := context.Background()

	before := snapshotFoldCount.Load()
	if _, _, err := tl.detail(ctx, "run1"); err != nil {
		t.Fatalf("first detail: %v", err)
	}
	seqBefore := currentLastSeq(tl)

	// Append a new step event; the tail folds it and bumps lastSeq → a new key.
	appendEvents(t, logPath, runDetailStepEvent(3, "run1.2", "run1", "rebase-check", "open"))
	waitForLastSeqAbove(t, tl, seqBefore)

	if _, _, err := tl.detail(ctx, "run1"); err != nil {
		t.Fatalf("second detail after append: %v", err)
	}
	if folds := snapshotFoldCount.Load() - before; folds != 2 {
		t.Fatalf("snapshot folds = %d, want 2 (a new fold generation must re-fold)", folds)
	}
}

// currentLastSeq reads the tailer's published fold cursor under its lock.
func currentLastSeq(tl *cityRunTailer) uint64 {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	return tl.lastSeq
}

// waitForLastSeqAbove blocks until the tailer's lastSeq advances past prev.
func waitForLastSeqAbove(t *testing.T, tl *cityRunTailer, prev uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if currentLastSeq(tl) > prev {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("lastSeq did not advance past %d within deadline (now %d)", prev, currentLastSeq(tl))
}
