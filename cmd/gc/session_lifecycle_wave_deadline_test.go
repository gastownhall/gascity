package main

import (
	"context"
	"errors"
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
	"github.com/gastownhall/gascity/internal/worker"
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
	wedgedStart  map[string]bool
	entered      chan string
	startEntered chan string
	release      chan struct{}
	once         sync.Once
	city         string // created before the cleanup that unblocks, so it outlives the released starts

	mu      sync.Mutex
	inProbe int // wedged probes entered and not yet released
	probes  map[string]int
}

type firstStartWaitsForTimeoutProvider struct {
	*runtime.Fake
	mu       sync.Mutex
	attempts int
}

func (p *firstStartWaitsForTimeoutProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.mu.Lock()
	p.attempts++
	attempt := p.attempts
	p.mu.Unlock()
	if attempt == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	return p.Fake.Start(ctx, name, cfg)
}

// newWedgedLivenessProvider wedges the named sessions and unblocks them when
// the test ends, so no start goroutine outlives it or the city directory it
// starts in.
func newWedgedLivenessProvider(t *testing.T, wedged ...string) *wedgedLivenessProvider {
	t.Helper()
	p := &wedgedLivenessProvider{
		Fake:         runtime.NewFake(),
		city:         t.TempDir(),
		wedged:       map[string]bool{},
		entered:      make(chan string, 16),
		startEntered: make(chan string, 16),
		release:      make(chan struct{}),
		probes:       map[string]int{},
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
		select {
		case p.startEntered <- name:
		default:
		}
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

func (p *wedgedLivenessProvider) waitStartEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.startEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("no start reached the wedged provider Start")
	}
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
	case <-time.After(10 * time.Second):
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
	return runStartWaveBounded(ctx, t, limit, sp.city, items, sp, startupTimeout, maxParallel)
}

func runStartWaveBounded(ctx context.Context, t *testing.T, limit time.Duration, city string, items []preparedStart, sp runtime.Provider, startupTimeout time.Duration, maxParallel int) ([]startResult, time.Duration) {
	t.Helper()
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

func TestExecutePreparedStartWave_StartupTimeoutCanRetryWithFreshWaveContext(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	sp := &firstStartWaitsForTimeoutProvider{Fake: runtime.NewFake()}
	items := []preparedStart{waveTestStart("retry")}
	const startupTimeout = 50 * time.Millisecond

	first, _ := runStartWaveBounded(context.Background(), t, 10*time.Second, t.TempDir(), items, sp, startupTimeout, 1)
	if len(first) != 1 || first[0].outcome != TraceOutcomeDeadlineExceeded || !errors.Is(first[0].err, context.DeadlineExceeded) {
		t.Fatalf("first wave result = %#v, want a startup timeout that can be retried", first)
	}
	second, _ := runStartWaveBounded(context.Background(), t, 10*time.Second, t.TempDir(), items, sp, startupTimeout, 1)
	if len(second) != 1 {
		t.Fatalf("second wave results = %d, want 1", len(second))
	}
	requireFinished(t, second[0], "retry")
	if calls := sp.CountCalls("Start", "retry"); calls != 1 {
		t.Fatalf("successful Provider.Start calls = %d, want 1 after retry", calls)
	}
}

func requireAbandoned(t *testing.T, r startResult, name string, mayHaveCalledStart bool) {
	t.Helper()
	if got := r.prepared.candidate.name(); got != name {
		t.Fatalf("abandoned result is for %q, want %q", got, name)
	}
	if !r.abandoned {
		t.Fatalf("result for %q: abandoned = false (outcome %q, err %v), want the wave to have stopped waiting", name, r.outcome, r.err)
	}
	if r.outcome != TraceOutcomeDeferred || r.err != nil || r.providerStartCalled {
		t.Fatalf("abandoned result for %q = outcome %q, err %v, providerStartCalled %v; want a Deferred with no error and no reported provider Start", name, r.outcome, r.err, r.providerStartCalled)
	}
	if r.providerStartMayHaveBeenCalled != mayHaveCalledStart {
		t.Fatalf("abandoned result for %q may-have-called-Start = %v, want %v", name, r.providerStartMayHaveBeenCalled, mayHaveCalledStart)
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
	results, elapsed := runWaveBounded(context.Background(), t, 10*time.Second, items, sp, startupTimeout, 2)

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
	requireAbandoned(t, results[1], "wedged", true)
}

// TestExecutePreparedStartWave_AbandonedProbeDoesNotReachStartAfterTheDeadline
// releases the blocked probe after the wave returned. The canceled wave context
// prevents the late liveness answer from crossing the start boundary, while a
// later wave gets a fresh context and can retry the session.
func TestExecutePreparedStartWave_AbandonedProbeDoesNotReachStartAfterTheDeadline(t *testing.T) {
	setWaveDeadlineSlack(t, 50*time.Millisecond)
	sp := newWedgedLivenessProvider(t, "wedged")
	results, _ := runWaveBounded(context.Background(), t, 10*time.Second, []preparedStart{waveTestStart("wedged")}, sp, 100*time.Millisecond, 1)
	requireAbandoned(t, results[0], "wedged", true)

	sp.unblock()
	waitStartGoroutinesAtMost(t, 0)
	if sp.IsRunning("wedged") {
		t.Fatal("the abandoned wave reached Provider.Start after its liveness probe returned")
	}

	requireAbandoned(t, results[0], "wedged", true)
	retry, _ := runStartWaveBounded(context.Background(), t, 10*time.Second, t.TempDir(), []preparedStart{waveTestStart("wedged")}, sp, 100*time.Millisecond, 1)
	if len(retry) != 1 {
		t.Fatalf("retry results = %d, want 1", len(retry))
	}
	requireFinished(t, retry[0], "wedged")
}

// TestExecutePreparedStartWave_CtxEndsTheWaitBeforeTheDeadline keeps the
// historical contract: caller cancellation ends the wait while an
// uncancellable Provider.Start holds the launched start, so may-have-called
// Start remains true. The deadline is an hour away.
func TestExecutePreparedStartWave_CtxEndsTheWaitBeforeTheDeadline(t *testing.T) {
	setWaveDeadlineSlack(t, time.Hour)
	sp := newWedgedLivenessProvider(t)
	sp.wedgedStart = map[string]bool{"wedged": true}
	items := []preparedStart{waveTestStart("healthy"), waveTestStart("wedged")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sp.waitStartEntered(t)
		sp.waitRunning(t, "healthy")
		// The healthy start has returned; its result reaches the collector
		// when its goroutine exits, leaving only the wedged one.
		waitStartGoroutinesAtMost(t, 1)
		cancel()
	}()
	results, elapsed := runWaveBounded(ctx, t, 10*time.Second, items, sp, time.Minute, 2)

	if elapsed > 10*time.Second {
		t.Fatalf("wave returned after %v, want promptly after the ctx ended", elapsed)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	requireFinished(t, results[0], "healthy")
	requireAbandoned(t, results[1], "wedged", true)
}

// TestExecutePreparedStartWave_CanceledContextPreservesCompletedUnansweredLiveness
// exercises the done-vs-cancel race: a same-key liveness observation is
// already wedged when the caller cancels, and the start reports its unanswered
// boundary result during the wave's grace period. The completed result keeps
// its unanswered marker but is not labeled abandoned because the wave received it.
func TestExecutePreparedStartWave_CanceledContextPreservesCompletedUnansweredLiveness(t *testing.T) {
	setWaveDeadlineSlack(t, time.Hour)
	sp := newWedgedLivenessProvider(t, "wedged")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sp.waitEntered(t)
		cancel()
	}()

	results, _ := runWaveBounded(ctx, t, 10*time.Second, []preparedStart{waveTestStart("wedged")}, sp, time.Minute, 1)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	got := results[0]
	if got.abandoned || !got.startBoundaryLivenessUnanswered || !got.storeOnly || got.outcome != TraceOutcomeDeferred || got.err != nil {
		t.Fatalf("canceled unanswered-liveness result = abandoned %v, boundary-unanswered %v, store-only %v, outcome %q, err %v; want a completed store-only Deferred result",
			got.abandoned, got.startBoundaryLivenessUnanswered, got.storeOnly, got.outcome, got.err)
	}
	if got.providerStartCalled || got.providerStartMayHaveBeenCalled {
		t.Fatalf("completed unknown-witness result reports provider Start: called=%v may-have-been-called=%v",
			got.providerStartCalled, got.providerStartMayHaveBeenCalled)
	}
	if calls := sp.CountCalls("Start", "wedged"); calls != 0 {
		t.Fatalf("Provider.Start calls = %d, want none after unanswered start-boundary liveness", calls)
	}
}

func TestExecutePlannedStartsCanceledWaveRestoresCompletedPreStartIdentity(t *testing.T) {
	setWaveDeadlineSlack(t, time.Hour)
	env := newReconcilerTestEnv(t)
	enableConditionalWritesForStartTest(t, env)
	priorCandidate, priorBead, priorTP := identityPlannerCandidate(t, env, "active")
	startIdentityRuntime(t, env, priorCandidate)
	priorMetadata := mustGetBead(t, env.store, priorBead.ID).Metadata
	maxRestarts := 5
	env.cfg.Daemon.SessionCircuitBreaker = true
	env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
	env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	cb := breakerAt(30*time.Minute, maxRestarts)
	setSessionCircuitBreakerForTest(t, cb)
	if err := setReconcilerDrainAckMetadata(env.sp, priorCandidate.name(), &drainState{reason: "orphaned", generation: 1}); err != nil {
		t.Fatalf("setting drain ack: %v", err)
	}
	sibling, siblingTP := identityPlannerSiblingCandidate(t, env, "session-b")
	before := env.sp.SnapshotCalls()

	slowStartEntered := make(chan struct{})
	releaseSlowStart := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		observeLivenessFn: func(name string, _ int, processNames []string) (runtime.Liveness, error) {
			if name == priorCandidate.name() {
				return runtime.Liveness{}, fmt.Errorf("prior pre-start probe: %w: %w", worker.ErrObservationUnanswered, runtime.ErrRuntimeUnavailable)
			}
			return runtime.ObserveLiveness(env.sp, name, processNames), nil
		},
		startFn: func(ctx context.Context, name string, cfg runtime.Config) error {
			if name != sibling.name() {
				return env.sp.Start(ctx, name, cfg)
			}
			startOnce.Do(func() { close(slowStartEntered) })
			<-releaseSlowStart
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("slow sibling start released before caller cancellation")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer releaseOnce.Do(func() { close(releaseSlowStart) })
	defer cancel()
	planned := make(chan int, 1)
	go func() {
		planned <- executePlannedStartsTraced(
			ctx,
			[]startCandidate{priorCandidate, sibling},
			env.cfg,
			map[string]TemplateParams{priorTP.SessionName: priorTP, siblingTP.SessionName: siblingTP},
			sp,
			env.store,
			"test-city",
			env.city,
			env.clk,
			events.Discard,
			time.Minute,
			&env.stdout,
			&env.stderr,
			nil,
			withWarmClaimProbe(alwaysUnclaimed),
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		)
	}()
	select {
	case <-slowStartEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("sibling did not reach its blocked Provider.Start")
	}
	waitStartGoroutinesAtMost(t, 1) // the earlier pre-start result is complete
	cancel()
	var woken int
	select {
	case woken = <-planned:
	case <-time.After(10 * time.Second):
		t.Fatal("sync planner did not finish after caller cancellation")
	}
	if woken != 0 {
		t.Fatalf("wake count = %d, want 0 for the completed deferral and unfinished sibling", woken)
	}

	assertNoProviderMutationCalls(t, env.sp.SnapshotCalls()[len(before):], priorCandidate.name(), sibling.name())
	gotPrior := mustGetBead(t, env.store, priorBead.ID).Metadata
	for _, key := range []string{
		"instance_token", "generation", "continuation_epoch", "continuation_reset_pending",
		"last_woke_at", "state", "sleep_reason", "detached_at", "wake_request",
		"wake_requested_at", "pending_create_started_at",
	} {
		if gotPrior[key] != priorMetadata[key] {
			t.Errorf("prior session %s = %q, want full pre-wake fence value %q", key, gotPrior[key], priorMetadata[key])
		}
	}
	gotSibling := env.sessionInfo(sibling.info.ID)
	if gotSibling.InstanceToken == sibling.info.InstanceToken || gotSibling.Generation == sibling.info.Generation {
		t.Fatalf("unfinished sibling restored its pre-wake identity: token=%q generation=%q", gotSibling.InstanceToken, gotSibling.Generation)
	}
	if !strings.Contains(env.stderr.String(), "outcome=start_boundary_liveness_unanswered") {
		t.Fatalf("stderr %q does not identify the completed unanswered pre-start result", env.stderr.String())
	}
	if !strings.Contains(env.stderr.String(), "session=session-b") || !strings.Contains(env.stderr.String(), "outcome=abandoned_at_wave") {
		t.Fatalf("stderr %q does not identify the unfinished sibling as abandoned", env.stderr.String())
	}
	requireSessionCircuitRestartCount(t, cb, env.store, priorBead.ID, namedSessionIdentityInfo(priorCandidate.info), env.clk.Now().UTC(), 0)
	requireSessionCircuitRestartCount(t, cb, env.store, sibling.info.ID, namedSessionIdentityInfo(sibling.info), env.clk.Now().UTC(), 1)

	releaseOnce.Do(func() { close(releaseSlowStart) })
	waitStartGoroutinesAtMost(t, 0)
}

// TestExecutePreparedStartWave_LaunchLoopBlockedOnTheSemaphoreIsAbandoned: with
// one slot and its start wedged, the second start never launches. The wave
// still returns at its deadline with both slots abandoned, and the second
// session's provider saw no call.
func TestExecutePreparedStartWave_LaunchLoopBlockedOnTheSemaphoreIsAbandoned(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	sp := newWedgedLivenessProvider(t, "first")
	first := waveTestStart("first")
	first.preWakeLivenessUnanswered = true
	first.preWakeUndo.written = sessionpkg.MetadataPatch{"last_woke_at": "first-attempt"}
	second := waveTestStart("second")
	second.preWakeLivenessUnanswered = true
	second.preWakeUndo.written = sessionpkg.MetadataPatch{"last_woke_at": "second-attempt"}
	items := []preparedStart{first, second}

	results, _ := runWaveBounded(context.Background(), t, 10*time.Second, items, sp, 200*time.Millisecond, 1)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	requireAbandoned(t, results[0], "first", true)
	requireAbandoned(t, results[1], "second", false)
	if results[0].restorePreWakeIdentity || results[0].restorePreWakeIdentityRequiresFence {
		t.Fatalf("launched abandoned start restore flags = (%v, %v), want false because Provider.Start may have been called",
			results[0].restorePreWakeIdentity, results[0].restorePreWakeIdentityRequiresFence)
	}
	if !results[1].restorePreWakeIdentity || !results[1].restorePreWakeIdentityRequiresFence {
		t.Fatalf("never-launched restore flags = (%v, %v), want fenced T1 restoration",
			results[1].restorePreWakeIdentity, results[1].restorePreWakeIdentityRequiresFence)
	}
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

			results, _ := runWaveBounded(context.Background(), t, 10*time.Second, items, sp, startupTimeout, 2)

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

func TestExecutePlannedStartsPreWakeSupersededPreservesMoverMarkerWithoutBreaker(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg.Daemon.SessionCircuitBreaker = false
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	const moverMarker = "2026-10-31T12:34:56Z"
	sp := &scriptedStartBoundaryProvider{
		Fake: env.sp,
		observeLivenessFn: func(string, int, []string) (runtime.Liveness, error) {
			// The planner has already taken and recorded its runtime lease before
			// this preflight probe. Model a mover taking over the row and assigning
			// its own in-flight marker before PreWake rereads it.
			if err := env.store.SetMetadataBatch(bead.ID, map[string]string{
				sessionpkg.RuntimeLeaseHolderKey: "mover/next-start",
				sessionpkg.RuntimeLeaseEpochKey:  "999",
				"last_woke_at":                   moverMarker,
			}); err != nil {
				return runtime.Liveness{}, err
			}
			return runtime.Liveness{}, nil
		},
	}

	if got := executePlannedStartsTraced(
		context.Background(), []startCandidate{candidate}, env.cfg,
		map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
		"test-city", env.city, env.clk, events.Discard, 100*time.Millisecond,
		&env.stdout, &env.stderr, nil,
		withWarmClaimProbe(alwaysUnclaimed),
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	); got != 0 {
		t.Fatalf("wake count = %d, want 0 after PreWake refused the moved lease", got)
	}
	metadata := mustGetBead(t, env.store, bead.ID).Metadata
	if metadata["last_woke_at"] != moverMarker {
		t.Fatalf("last_woke_at = %q, want mover's marker %q", metadata["last_woke_at"], moverMarker)
	}
	if metadata[sessionpkg.RuntimeLeaseHolderKey] != "mover/next-start" || metadata[sessionpkg.RuntimeLeaseEpochKey] != "999" {
		t.Fatalf("runtime lease after superseded PreWake = (%q, %q), want the mover's holder and epoch", metadata[sessionpkg.RuntimeLeaseHolderKey], metadata[sessionpkg.RuntimeLeaseEpochKey])
	}
	if starts := env.sp.CountCalls("Start", candidate.name()); starts != 0 {
		t.Fatalf("Provider.Start calls = %d, want none after PreWake refused the moved lease", starts)
	}
}

func TestExecutePlannedStartsCanceledBeforeLaunchRestoresUnansweredPreWakeT1(t *testing.T) {
	for _, tc := range []struct {
		name  string
		async bool
		cas   bool
	}{
		{name: "sync CAS", cas: true},
		{name: "async CAS", async: true, cas: true},
		{name: "sync no CAS"},
		{name: "async no CAS", async: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			if tc.cas {
				enableConditionalWritesForStartTest(t, env)
			}
			writer, _, err := beads.ResolveConditionalWriter(env.store)
			if err != nil {
				t.Fatalf("resolving conditional writer: %v", err)
			}
			if (writer != nil) != tc.cas {
				t.Fatalf("conditional writer available = %v, want %v", writer != nil, tc.cas)
			}
			candidate, bead, tp := identityPlannerCandidate(t, env, "active")
			sp := newWedgedLivenessProvider(t, candidate.name())
			sp.city = env.city
			env.sp = sp.Fake
			startIdentityRuntime(t, env, candidate)
			if !sp.Fake.IsRunning(candidate.name()) {
				t.Fatalf("T1 runtime %q did not start before the planner was canceled", candidate.name())
			}
			prior := mustGetBead(t, env.store, bead.ID).Metadata

			maxRestarts := 5
			env.cfg.Daemon.SessionCircuitBreaker = true
			env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
			env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
			identity := namedSessionIdentityInfo(candidate.info)
			cb := breakerAt(30*time.Minute, maxRestarts)
			setSessionCircuitBreakerForTest(t, cb)

			providerCallsBefore := sp.Fake.SnapshotCalls()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			options := []startExecutionOption{
				withWarmClaimProbe(alwaysUnclaimed),
				withStartStabilityWaiter(immediateStartStabilityWaiter),
				withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
			}
			var tracker asyncStartTracker
			if tc.async {
				options = append(options,
					withAsyncStartExecution(),
					withAsyncStartLimiter(newAsyncStartLimiter(1)),
					withAsyncStartTracker(&tracker),
				)
			}
			planned := make(chan int, 1)
			go func() {
				planned <- executePlannedStartsTraced(
					ctx, []startCandidate{candidate}, env.cfg,
					map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
					"test-city", env.city, env.clk, events.Discard, time.Minute,
					&env.stdout, &env.stderr, nil, options...,
				)
			}()
			sp.waitEntered(t)
			cancel()
			select {
			case got := <-planned:
				if got != 0 {
					t.Fatalf("wake count = %d, want 0 after cancellation before start launch", got)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("planner did not finish after cancellation during the wedged pre-PreWake probe")
			}
			if tc.async && !tracker.wait(10*time.Second) {
				t.Fatal("async reservation was not released for the never-enqueued start")
			}

			if probes := sp.probeCount(candidate.name()); probes != 1 {
				t.Fatalf("pre-PreWake provider probes = %d, want the single genuinely wedged probe", probes)
			}
			callsAfter := sp.Fake.SnapshotCalls()
			assertNoProviderMutationCalls(t, callsAfter[len(providerCallsBefore):], candidate.name())
			for _, call := range callsAfter[len(providerCallsBefore):] {
				if call.Name == candidate.name() && call.Method == "Start" {
					t.Fatalf("Provider.Start(%q) ran after cancellation before launch", candidate.name())
				}
			}
			if starts := sp.CountCalls("Start", candidate.name()); starts != 1 {
				t.Fatalf("Provider.Start calls = %d, want only the pre-existing T1 runtime", starts)
			}
			if !sp.Fake.IsRunning(candidate.name()) {
				t.Fatalf("pre-existing T1 runtime %q was stopped during cancellation cleanup", candidate.name())
			}

			got := mustGetBead(t, env.store, bead.ID).Metadata
			lifecycleKeys := []string{
				"state", "sleep_reason", "detached_at", "continuation_reset_pending",
				"wake_request", "wake_requested_at", "pending_create_started_at",
			}
			for _, key := range lifecycleKeys {
				if got[key] != prior[key] {
					t.Errorf("%s = %q, want pre-wake lifecycle value %q", key, got[key], prior[key])
				}
			}
			if tc.cas {
				for _, key := range []string{"generation", "instance_token", "continuation_epoch", "last_woke_at"} {
					if got[key] != prior[key] {
						t.Errorf("CAS restore %s = %q, want T1 value %q", key, got[key], prior[key])
					}
				}
			} else {
				if got["generation"] == prior["generation"] || got["instance_token"] == prior["instance_token"] {
					t.Fatalf("no-CAS fallback restored T1 identity: generation/token = %q/%q, prior %q/%q", got["generation"], got["instance_token"], prior["generation"], prior["instance_token"])
				}
				if got["last_woke_at"] != "" {
					t.Fatalf("no-CAS lifecycle cleanup left last_woke_at = %q, want cleared", got["last_woke_at"])
				}
			}
			requireSessionCircuitRestartCount(t, cb, env.store, bead.ID, identity, env.clk.Now().UTC(), 0)
		})
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
	case <-time.After(10 * time.Second):
		t.Fatal("the synchronous planner had not returned after 5s: its wave waits on a start nothing can cancel")
		return 0
	}
}

// TestExecutePlannedStarts_SyncWaveAbandonedStartTouchesNoRuntime: the planner
// runs into a genuinely wedged same-city preflight observation. Later
// start-boundary/retry probes see it as outstanding, so it commits a store-only
// deferral: no provider metadata, drain-ack clear or stale stop, the
// pending-start lease is cleared, no failure or quarantine accrues, and stderr
// names the bead.
func TestExecutePlannedStarts_SyncWaveAbandonedStartTouchesNoRuntime(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	f := newWedgedPlannerFixture(t)

	if got := f.plan(t, f.cand); got != 0 {
		t.Fatalf("woken = %d, want 0 for an abandoned start; log %q", got, f.log.String())
	}

	for _, call := range f.wedged.SnapshotCalls() {
		if call.Name == "worker" {
			switch call.Method {
			case "GetMeta", "SetMeta", "RemoveMeta", "Stop", "Start":
				t.Fatalf("provider call %s(%q, %q) for unanswered start-boundary liveness; the planner must not touch a suspect runtime", call.Method, call.Name, call.Key)
			}
		}
	}
	if probes := f.wedged.probeCount("worker"); probes != 1 {
		t.Fatalf("same-city liveness probes = %d, want only the genuinely wedged preflight probe", probes)
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
		t.Fatalf("stderr %q does not name the unanswered start's bead %s", f.log.String(), f.cand.info.ID)
	}
	if !strings.Contains(f.log.String(), "outcome=start_boundary_liveness_unanswered") {
		t.Fatalf("stderr %q does not identify the unanswered start-boundary observation", f.log.String())
	}
}

// TestExecutePlannedStarts_AbandonedProviderStartCommitDoesNotWaitForMutationLock
// holds the session mutation lock inside StartRuntimeOnly while the wave gives
// up waiting. The store-only deferral must return before the provider call is
// released; restoring PreWake would wait on the same lock forever.
func TestExecutePlannedStarts_AbandonedProviderStartCommitDoesNotWaitForMutationLock(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	f := newWedgedPlannerFixture(t)
	f.wedged.wedged = map[string]bool{}
	f.wedged.wedgedStart = map[string]bool{"worker": true}
	woken := make(chan int, 1)
	go func() {
		woken <- executePlannedStartsTraced(context.Background(), []startCandidate{f.cand}, f.cfg, f.desired, f.wedged, f.store,
			"test-city", f.city, &clock.Fake{Time: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)}, events.Discard, 100*time.Millisecond, &f.log, &f.log, nil)
	}()
	f.wedged.waitStartEntered(t)
	select {
	case got := <-woken:
		if got != 0 {
			t.Fatalf("woken = %d, want 0 for an abandoned provider Start", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("abandoned start commit waited on the mutation lock held by Provider.Start")
	}
	for _, call := range f.wedged.SnapshotCalls() {
		if call.Name != "worker" {
			continue
		}
		switch call.Method {
		case "RemoveMeta", "SetMeta", "Stop":
			t.Fatalf("provider call %s(%q, %q) for a wave-abandoned start; the planner must not touch the runtime", call.Method, call.Name, call.Key)
		}
	}
	if !strings.Contains(f.log.String(), "outcome=abandoned_at_wave") {
		t.Fatalf("stderr %q does not identify the start abandoned at the wave deadline", f.log.String())
	}
	current := mustGetBead(t, f.store, f.cand.info.ID).Metadata
	if current["instance_token"] == f.cand.info.InstanceToken || current["generation"] == f.cand.info.Generation {
		t.Fatalf("abandoned start restored its pre-wake identity while Start may be in flight: %v", current)
	}
	f.wedged.unblock()
	f.wedged.waitRunning(t, "worker")
	waitStartGoroutinesAtMost(t, 0)
}

func TestExecutePlannedStarts_AbandonedProviderStartKeepsCircuitRestart(t *testing.T) {
	setWaveDeadlineSlack(t, 100*time.Millisecond)
	env := newReconcilerTestEnv(t)
	candidate, bead, tp := identityPlannerCandidate(t, env, "active")
	maxRestarts := 5
	env.cfg.Daemon.SessionCircuitBreaker = true
	env.cfg.Daemon.SessionCircuitBreakerMaxRestarts = &maxRestarts
	env.cfg.Daemon.SessionCircuitBreakerWindow = "30m"
	identity := namedSessionIdentityInfo(candidate.info)
	cb := breakerAt(30*time.Minute, maxRestarts)
	setSessionCircuitBreakerForTest(t, cb)

	sp := newWedgedLivenessProvider(t)
	sp.wedgedStart = map[string]bool{candidate.name(): true}
	woken := make(chan int, 1)
	go func() {
		woken <- executePlannedStartsTraced(context.Background(), []startCandidate{candidate}, env.cfg,
			map[string]TemplateParams{tp.SessionName: tp}, sp, env.store,
			"test-city", env.city, env.clk, events.Discard, 100*time.Millisecond,
			&env.stdout, &env.stderr, nil,
			withWarmClaimProbe(alwaysUnclaimed),
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		)
	}()
	sp.waitStartEntered(t)
	select {
	case got := <-woken:
		if got != 0 {
			t.Fatalf("woken = %d, want 0 for a wave-abandoned Provider.Start", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("planner did not finish after abandoning Provider.Start")
	}
	assertRestartCount := func(label string) {
		t.Helper()
		found := false
		for _, snapshot := range cb.Snapshot(env.clk.Now().UTC()) {
			if snapshot.Identity == identity {
				found = true
				if snapshot.RestartCount != 1 {
					t.Fatalf("in-memory restart count %s abandonment = %d, want 1", label, snapshot.RestartCount)
				}
			}
		}
		if !found {
			t.Fatalf("in-memory breaker has no entry after %s abandonment", label)
		}
		state, err := sessionFrontDoor(env.store).CircuitState(bead.ID)
		if err != nil {
			t.Fatalf("reading persisted restart count after %s abandonment: %v", label, err)
		}
		restarts, err := parseCircuitTimeList(state.Restarts)
		if err != nil || len(restarts) != 1 {
			t.Fatalf("persisted restart count after %s abandonment = %d (err %v), want 1", label, len(restarts), err)
		}
	}
	assertRestartCount("before the late Provider.Start returns")

	sp.unblock()
	sp.waitRunning(t, candidate.name())
	waitStartGoroutinesAtMost(t, 0)
	assertRestartCount("after the launched Provider.Start returns")
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

// TestWaveDeadlineSlackOutgrowsEveryStartPathBound fails when a bound on a
// start path outgrows the wave deadline. The provider-start path includes the
// pre-Start liveness observation and its longest serial post-start chain.
// The session-layer reuse path's unavailable identity branch returns
// immediately, so it does not run retry attribution; include only reachable
// serial waits.
func TestWaveDeadlineSlackOutgrowsEveryStartPathBound(t *testing.T) {
	type boundedWait struct {
		name  string
		bound time.Duration
	}
	paths := []struct {
		name  string
		waits []boundedWait
	}{
		{
			name: "provider start dies after state-sync recovery and converges",
			waits: []boundedWait{
				{"pre-Start liveness observation", postStartObservationBound},
				{"ErrStateSync recovery observation", postStartObservationBound},
				{"post-startup liveness observation", postStartObservationBound},
				{"post-start death pane", postStartObservationBound},
				{"failure probes (rate-limit peek and pending-create identity)", startFailureProbeBudget},
				{"started runtime binding after attributed convergence", postStartObservationBound},
			},
		},
		{
			name: "provider start converges after post-start observations",
			waits: []boundedWait{
				{"pre-Start liveness observation", postStartObservationBound},
				{"ErrStateSync recovery observation", postStartObservationBound},
				{"post-startup liveness observation", postStartObservationBound},
				{"started runtime binding", postStartObservationBound},
			},
		},
		{
			name: "session-exists collision converges after retry probes",
			waits: []boundedWait{
				{"pre-Start liveness observation", postStartObservationBound},
				{"collision liveness and identity", startFailureProbeBudget},
				{"failed-start probes after collision", startFailureProbeBudget},
				{"started runtime binding after convergence", postStartObservationBound},
			},
		},
		{
			name: "unverifiable start-boundary identity returns store-only",
			waits: []boundedWait{
				{"pre-Start liveness observation", postStartObservationBound},
				{"start-boundary liveness and identity observation", postStartObservationBound},
			},
		},
		{
			name: "unavailable start-boundary liveness converges on retry",
			waits: []boundedWait{
				{"pre-Start liveness observation", postStartObservationBound},
				{"retry attribution after liveness-unavailable", startFailureProbeBudget},
				{"started runtime binding after retry", postStartObservationBound},
			},
		},
	}
	var longest time.Duration
	var longestPath string
	for _, path := range paths {
		var total time.Duration
		for _, wait := range path.waits {
			total += wait.bound
		}
		if total > longest {
			longest = total
			longestPath = path.name
		}
	}
	if waveDeadlineSlack <= longest {
		t.Fatalf("waveDeadlineSlack = %v, want more than the longest serial chain (%s, %v)", waveDeadlineSlack, longestPath, longest)
	}
}

func TestStartRuntimeLeaseSafeUntilCoversPreparationAndWaveWindow(t *testing.T) {
	startupTimeout := 2 * time.Minute
	for _, prepareBudget := range []time.Duration{0, defaultStartPrepareBudget, defaultStartPrepareBudget + 20*time.Second} {
		prepareWindow := max(defaultStartPrepareBudget, prepareBudget)
		started := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
		expires := started.Add(startRuntimeLeaseTTL(startupTimeout, prepareBudget))
		safeUntil := expires.Add(-sessionpkg.RuntimeLeaseSkewAllowance)
		waveEnd := started.Add(prepareWindow + startupTimeout + waveDeadlineSlack)
		wantSafeUntil := waveEnd.Add(sessionpkg.RuntimeLeaseMargin)
		if safeUntil.Before(wantSafeUntil) {
			t.Fatalf("prepare budget %s: SafeUntil %s, want at least wave end plus lease margin %s (batch size %d)",
				prepareBudget, safeUntil, wantSafeUntil, defaultStartDependencyRecheckBatchSize)
		}
	}
}
