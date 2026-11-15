package main

import (
	"context"
	"fmt"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wedgedLivenessProvider blocks the liveness probe startPreparedStartCandidate
// makes first, a call that takes no ctx and that nothing can cancel, for the
// named sessions until unblock. It stands in for a runtime whose tmux
// subprocess or store has hung. Every other call is the Fake's.
type wedgedLivenessProvider struct {
	*runtime.Fake
	wedged map[string]bool
	// wedgedStart names sessions whose provider Start (not the probe before
	// it) blocks until release and then completes, ignoring ctx.
	wedgedStart map[string]bool
	entered     chan string
	release     chan struct{}
	once        sync.Once
	city        string // created before the cleanup that unblocks, so it outlives the released starts

	mu      sync.Mutex
	inProbe int // wedged probes entered and not yet released
	probes  map[string]int
}

// newWedgedLivenessProvider wedges the named sessions and unblocks them when
// the test ends, so no start goroutine outlives it or the city directory it
// starts in.
func newWedgedLivenessProvider(t *testing.T, wedged ...string) *wedgedLivenessProvider {
	t.Helper()
	p := &wedgedLivenessProvider{
		Fake:    runtime.NewFake(),
		city:    t.TempDir(),
		wedged:  map[string]bool{},
		entered: make(chan string, 16),
		release: make(chan struct{}),
		probes:  map[string]int{},
	}
	for _, name := range wedged {
		p.wedged[name] = true
	}
	t.Cleanup(func() {
		p.unblock()
		waitStartGoroutinesAtMost(t, 0)
	})
	return p
}

func (p *wedgedLivenessProvider) unblock() { p.once.Do(func() { close(p.release) }) }

//nolint:unparam // implements runtime.LivenessObserverWithError; the double never errors.
func (p *wedgedLivenessProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	p.mu.Lock()
	p.probes[name]++
	p.mu.Unlock()
	if p.wedged[name] {
		p.mu.Lock()
		p.inProbe++
		p.mu.Unlock()
		select {
		case p.entered <- name:
		default:
		}
		<-p.release
		p.mu.Lock()
		p.inProbe--
		p.mu.Unlock()
	}
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

// Start blocks for a session in wedgedStart, as a provider call that takes no
// ctx would, and then starts it on the Fake.
func (p *wedgedLivenessProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.wedgedStart[name] {
		p.mu.Lock()
		p.inProbe++
		p.mu.Unlock()
		<-p.release
		p.mu.Lock()
		p.inProbe--
		p.mu.Unlock()
	}
	return p.Fake.Start(ctx, name, cfg)
}

func (p *wedgedLivenessProvider) probeCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.probes[name]
}

// waitEntered blocks until a wedged probe has been entered, so the test knows
// the start goroutine is stuck rather than not yet running.
func (p *wedgedLivenessProvider) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no start reached the wedged liveness probe")
	}
}

// waitRunning blocks until the Fake reports name running, that is until the
// provider Start of a released goroutine has returned.
func (p *wedgedLivenessProvider) waitRunning(t *testing.T, name string) {
	t.Helper()
	pollUntil(t, fmt.Sprintf("session %q started after the release", name), func() bool { return p.IsRunning(name) })
}

// startGoroutines counts the goroutines still inside runPreparedStartCandidate,
// the wave's start goroutines: one that has left it has written its result and
// released its slot, and its exit is the only completion signal the wave
// offers a caller that has stopped waiting.
func startGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := goruntime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "runPreparedStartCandidate(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// waitStartGoroutinesAtMost blocks until no more than n start goroutines are
// still running: the wedged ones the test has not released.
func waitStartGoroutinesAtMost(t *testing.T, n int) {
	t.Helper()
	pollUntil(t, fmt.Sprintf("at most %d start goroutines left", n), func() bool { return startGoroutines() <= n })
}

// pollUntil is this file's one polling boundary: the wave's goroutines expose
// no completion signal to a caller that has stopped waiting for them, so it
// checks cond on a ticker until a safety deadline.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for: %s", what)
		case <-tick.C:
		}
	}
}

// setWaveDeadlineSlack shrinks waveDeadlineSlack for one test. It mutates a
// package var, so its callers must not run in parallel.
func setWaveDeadlineSlack(t *testing.T, d time.Duration) {
	t.Helper()
	prev := waveDeadlineSlack
	waveDeadlineSlack = d
	t.Cleanup(func() { waveDeadlineSlack = prev })
}

func waveTestStart(name string) preparedStart {
	return preparedStart{
		candidate: startCandidate{
			info: sessionpkg.Info{SessionName: name, SessionNameMetadata: name, Template: "worker"},
			tp:   TemplateParams{Command: "claude", SessionName: name, TemplateName: "worker"},
		},
		cfg: runtime.Config{Command: "claude"},
	}
}

// runWaveBounded runs the wave on its own goroutine and fails the test, rather
// than hanging the test binary, if it has not returned within limit.
func runWaveBounded(ctx context.Context, t *testing.T, limit time.Duration, items []preparedStart, sp *wedgedLivenessProvider, startupTimeout time.Duration, maxParallel int) ([]startResult, time.Duration) {
	t.Helper()
	city := sp.city
	type waveReturn struct {
		results []startResult
		elapsed time.Duration
	}
	ch := make(chan waveReturn, 1)
	go func() {
		begin := time.Now()
		results := executePreparedStartWaveForCity(ctx, items, city, sp, nil, nil, startupTimeout, maxParallel)
		ch <- waveReturn{results: results, elapsed: time.Since(begin)}
	}()
	select {
	case got := <-ch:
		return got.results, got.elapsed
	case <-time.After(limit):
		t.Fatalf("the wave had not returned after %v: its collector waits on a start nothing can cancel", limit)
		return nil, 0
	}
}

func requireAbandoned(t *testing.T, r startResult, name string) {
	t.Helper()
	if got := r.prepared.candidate.name(); got != name {
		t.Fatalf("abandoned result is for %q, want %q", got, name)
	}
	if !r.abandoned {
		t.Fatalf("result for %q: abandoned = false (outcome %q, err %v), want the wave to have stopped waiting", name, r.outcome, r.err)
	}
	if r.outcome != TraceOutcomeDeferred || r.err != nil || r.providerStartCalled {
		t.Fatalf("abandoned result for %q = outcome %q, err %v, providerStartCalled %v; want a Deferred with no error and no provider Start", name, r.outcome, r.err, r.providerStartCalled)
	}
}

func requireFinished(t *testing.T, r startResult, name string) {
	t.Helper()
	if got := r.prepared.candidate.name(); got != name {
		t.Fatalf("result is for %q, want %q", got, name)
	}
	if r.abandoned || r.err != nil || r.outcome != TraceOutcomeSuccess {
		t.Fatalf("result for %q = abandoned %v, outcome %q, err %v; want its real success", name, r.abandoned, r.outcome, r.err)
	}
}

// TestExecutePreparedStartWave_WedgedStartIsAbandonedAtTheWaveDeadline: one
// start blocks in a call nothing can cancel. The wave returns at its deadline,
// in input order, with the healthy start's real result and the blocked
// start as a deferral.
func TestExecutePreparedStartWave_WedgedStartIsAbandonedAtTheWaveDeadline(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	sp := newWedgedLivenessProvider(t, "wedged")
	items := []preparedStart{waveTestStart("healthy"), waveTestStart("wedged")}

	const startupTimeout = 200 * time.Millisecond
	results, elapsed := runWaveBounded(context.Background(), t, 5*time.Second, items, sp, startupTimeout, 2)

	if want := startupTimeout + waveDeadlineSlack; elapsed < want {
		t.Fatalf("wave returned after %v, before its deadline %v", elapsed, want)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("wave returned after %v, want about its deadline %v", elapsed, startupTimeout+waveDeadlineSlack)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	requireFinished(t, results[0], "healthy")
	requireAbandoned(t, results[1], "wedged")
}

// TestExecutePreparedStartWave_AbandonedStartCompletingLateDoesNotRaceTheCaller
// releases the blocked start after the wave returned: its late write must go
// to the wave's own slice, never to the one the caller reads. A guard; the
// wedged-start test above carries the RED.
func TestExecutePreparedStartWave_AbandonedStartCompletingLateDoesNotRaceTheCaller(t *testing.T) {
	setWaveDeadlineSlack(t, 50*time.Millisecond)
	sp := newWedgedLivenessProvider(t, "wedged")
	results, _ := runWaveBounded(context.Background(), t, 5*time.Second, []preparedStart{waveTestStart("wedged")}, sp, 100*time.Millisecond, 1)
	requireAbandoned(t, results[0], "wedged")

	sp.unblock()
	sp.waitRunning(t, "wedged")
	// Start has returned; its goroutine writes its result before it exits.
	waitStartGoroutinesAtMost(t, 0)

	requireAbandoned(t, results[0], "wedged")
}

// TestExecutePreparedStartWave_CtxEndsTheWaitBeforeTheDeadline: a canceled
// ctx ends the wait on its own, with the deadline an hour away.
func TestExecutePreparedStartWave_CtxEndsTheWaitBeforeTheDeadline(t *testing.T) {
	setWaveDeadlineSlack(t, time.Hour)
	sp := newWedgedLivenessProvider(t, "wedged")
	items := []preparedStart{waveTestStart("healthy"), waveTestStart("wedged")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sp.waitEntered(t)
		sp.waitRunning(t, "healthy")
		// The healthy start has returned; its result reaches the collector
		// when its goroutine exits, leaving only the wedged one.
		waitStartGoroutinesAtMost(t, 1)
		cancel()
	}()
	results, elapsed := runWaveBounded(ctx, t, 10*time.Second, items, sp, time.Minute, 2)

	if elapsed > 5*time.Second {
		t.Fatalf("wave returned after %v, want promptly after the ctx ended", elapsed)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	requireFinished(t, results[0], "healthy")
	requireAbandoned(t, results[1], "wedged")
}

// TestExecutePreparedStartWave_LaunchLoopBlockedOnTheSemaphoreIsAbandoned: with
// one slot and its start wedged, the second start never launches. The wave
// still returns at its deadline with both slots abandoned, and the second
// session's provider saw no call.
func TestExecutePreparedStartWave_LaunchLoopBlockedOnTheSemaphoreIsAbandoned(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	sp := newWedgedLivenessProvider(t, "first")
	items := []preparedStart{waveTestStart("first"), waveTestStart("second")}

	results, _ := runWaveBounded(context.Background(), t, 5*time.Second, items, sp, 200*time.Millisecond, 1)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	requireAbandoned(t, results[0], "first")
	requireAbandoned(t, results[1], "second")
	if n := sp.probeCount("second"); n != 0 {
		t.Fatalf("the second start's provider saw %d liveness probes, want 0: it was never launched", n)
	}
}

// TestExecutePreparedStartWave_HealthyWaveIsUnchangedByTheDeadline: a wave whose
// starts all finish returns their real results in input order, with a startup
// timeout and without one (no deadline).
func TestExecutePreparedStartWave_HealthyWaveIsUnchangedByTheDeadline(t *testing.T) {
	for name, startupTimeout := range map[string]time.Duration{"with a startup timeout": 10 * time.Second, "without one": 0} {
		t.Run(name, func(t *testing.T) {
			sp := newWedgedLivenessProvider(t)
			items := []preparedStart{waveTestStart("a"), waveTestStart("b"), waveTestStart("c")}

			results, _ := runWaveBounded(context.Background(), t, 5*time.Second, items, sp, startupTimeout, 2)

			if len(results) != 3 {
				t.Fatalf("got %d results, want 3", len(results))
			}
			for i, want := range []string{"a", "b", "c"} {
				requireFinished(t, results[i], want)
			}
		})
	}
}

// wedgedPlannerFixture is one creating session driven through the synchronous
// planner against a provider whose liveness probe for it is wedged.
type wedgedPlannerFixture struct {
	*leaseStartFixture
	wedged *wedgedLivenessProvider
}

func newWedgedPlannerFixture(t *testing.T) *wedgedPlannerFixture {
	t.Helper()
	return &wedgedPlannerFixture{
		leaseStartFixture: newLeaseStartFixture(t, beads.NewMemStore()),
		wedged:            newWedgedLivenessProvider(t, "worker"),
	}
}

// plan runs the synchronous planner over the fixture's candidate on its own
// goroutine and fails, rather than hangs, if it does not return.
func (f *wedgedPlannerFixture) plan(t *testing.T, cand startCandidate) int {
	t.Helper()
	clk := &clock.Fake{Time: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}
	woken := make(chan int, 1)
	go func() {
		woken <- executePlannedStartsTraced(context.Background(), []startCandidate{cand}, f.cfg, f.desired, f.wedged, f.store,
			"test-city", f.city, clk, events.Discard, 100*time.Millisecond, &f.log, &f.log, nil)
	}()
	select {
	case n := <-woken:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("the synchronous planner had not returned after 5s: its wave waits on a start nothing can cancel")
		return 0
	}
}

// TestExecutePlannedStarts_SyncWaveAbandonedStartTouchesNoRuntime: the planner
// commits an abandoned start as a store-only deferral. It makes no provider
// call by name for it (no drain-ack clear, no stale stop), clears the
// pending-start lease, accrues no failure or quarantine, and says so on stderr.
func TestExecutePlannedStarts_SyncWaveAbandonedStartTouchesNoRuntime(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	f := newWedgedPlannerFixture(t)

	if got := f.plan(t, f.cand); got != 0 {
		t.Fatalf("woken = %d, want 0 for an abandoned start; log %q", got, f.log.String())
	}

	for _, call := range f.wedged.SnapshotCalls() {
		if call.Name == "worker" && (call.Method == "RemoveMeta" || call.Method == "Stop") {
			t.Fatalf("provider call %s(%q, %q) for the abandoned start; the planner must not touch a suspect runtime", call.Method, call.Name, call.Key)
		}
	}
	meta := mustGetBead(t, f.store, f.cand.info.ID).Metadata
	if meta["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want the pending-start lease cleared", meta["last_woke_at"])
	}
	for _, key := range []string{"wake_attempts", "quarantined_until", "sleep_reason"} {
		if v := meta[key]; v != "" && v != "0" {
			t.Fatalf("%s = %q, want no failure accounting for an abandoned start; metadata %v", key, v, meta)
		}
	}
	if !strings.Contains(f.log.String(), f.cand.info.ID) {
		t.Fatalf("stderr %q does not name the abandoned start's bead %s", f.log.String(), f.cand.info.ID)
	}
}

// TestExecutePlannedStarts_SyncWaveAbandonedStartCompletingLate is a
// characterization, not new behavior: the wave abandons a start, the start
// later completes and leaves a runtime behind, and the next planner pass over
// the same candidate must neither start a duplicate nor wedge the bead.
func TestExecutePlannedStarts_SyncWaveAbandonedStartCompletingLate(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	f := newWedgedPlannerFixture(t)
	// The start itself wedges, so it completes after the release and leaves a
	// runtime behind (a wedged probe would resume on an expired startup ctx).
	f.wedged.wedged = map[string]bool{}
	f.wedged.wedgedStart = map[string]bool{"worker": true}
	if got := f.plan(t, f.cand); got != 0 {
		t.Fatalf("first pass woken = %d, want 0", got)
	}

	f.wedged.unblock()
	f.wedged.waitRunning(t, "worker")
	waitStartGoroutinesAtMost(t, 0)

	next := f.cand
	next.info = sessiontest.SeedBead(t, mustGetBead(t, f.store, f.cand.info.ID))
	woken := f.plan(t, next)

	after := mustGetBead(t, f.store, f.cand.info.ID).Metadata
	t.Logf("second pass: woken=%d starts=%d state=%q pending_create_claim=%q wake_attempts=%q\nlog:\n%s",
		woken, f.wedged.CountCalls("Start", "worker"), after["state"], after["pending_create_claim"], after["wake_attempts"], f.log.String())
	if n := f.wedged.CountCalls("Start", "worker"); n != 1 {
		t.Fatalf("provider Start ran %d times for the session, want 1: the next pass must adopt the late start, not duplicate it", n)
	}
}

// TestWaveDeadlineSlackOutgrowsEveryStartPathBound fails when a bound on the
// start path outgrows the wave deadline: a healthy slow start finishes inside
// startupTimeout plus the longest serial chain of waits that follows Start, and
// the slack must clear that chain or the wave abandons starts that were fine.
// Add a bound to the list when a new bounded wait lands on the start path.
func TestWaveDeadlineSlackOutgrowsEveryStartPathBound(t *testing.T) {
	chain := []struct {
		name  string
		bound time.Duration
	}{
		{"ErrStateSync recovery observation", postStartObservationBound},
		{"post-startup liveness observation", postStartObservationBound},
		{"failure probes (rate-limit peek and pending-create identity)", startFailureProbeBudget},
	}
	var total time.Duration
	for _, step := range chain {
		total += step.bound
	}
	if waveDeadlineSlack <= total {
		t.Fatalf("waveDeadlineSlack = %v, want more than the serial chain of waits after Start (%v)", waveDeadlineSlack, total)
	}
}
