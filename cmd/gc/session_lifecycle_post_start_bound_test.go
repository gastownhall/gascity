package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

const (
	// postStartBound is the bound the start path puts on each provider call it
	// makes after provider.Start. These tests run on synctest's fake clock, so
	// it costs no wall time.
	postStartBound = 10 * time.Second
	// postStartWatchdog is far past postStartBound. It only turns an unbounded
	// wait into a test failure instead of a deadlocked bubble.
	postStartWatchdog = 5 * time.Minute
	// postStartTimeout is the startup_timeout these scenarios run under: clear
	// of postStartBound, so the bound under test is the only deadline in play.
	postStartTimeout = time.Minute
)

// postStartProvider is a runtime whose provider calls can hang the way a wedged
// tmux subprocess does: a hung call ignores its context and returns only when
// release is called, and then answers normally. Observation hangs are picked by
// ordinal, so a scenario lets the preflight answer and hangs the one under test.
type postStartProvider struct {
	*runtime.Fake
	// startErr is what Start returns once the runtime exists and carries its
	// identity: the shape of a start that failed late.
	startErr error

	// hangObservation is the 1-based ordinal, per attempt, of the
	// ObserveLivenessWithError call that hangs. Zero hangs none.
	hangObservation atomic.Int64
	hangPeek        atomic.Bool
	hangGetMeta     atomic.Bool
	// peekDelay is how long Peek takes to answer, in nanoseconds.
	peekDelay atomic.Int64
	// startUntilDeadline makes Start wait for its context to end and then
	// report that the session already exists, without creating a runtime.
	startUntilDeadline atomic.Bool

	observations atomic.Int64
	// hangs counts the calls that ever hung, hanging the ones hung right now.
	hangs       atomic.Int64
	hanging     atomic.Int64
	peakHanging atomic.Int64

	mu        sync.Mutex
	hungCalls []string

	releaseC    chan struct{}
	releaseOnce sync.Once
}

func newPostStartProvider() *postStartProvider {
	return &postStartProvider{Fake: runtime.NewFake(), releaseC: make(chan struct{})}
}

// hang blocks the calling provider call until release, recording call as the
// one that hung.
func (p *postStartProvider) hang(call string) {
	p.mu.Lock()
	p.hungCalls = append(p.hungCalls, call)
	p.mu.Unlock()
	p.hangs.Add(1)
	n := p.hanging.Add(1)
	defer p.hanging.Add(-1)
	for {
		peak := p.peakHanging.Load()
		if n <= peak || p.peakHanging.CompareAndSwap(peak, n) {
			break
		}
	}
	<-p.releaseC
}

// hungIn names every provider call that has hung, for failure messages: it
// shows which wait the start path is stuck on.
func (p *postStartProvider) hungIn() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.hungCalls) == 0 {
		return "none"
	}
	return strings.Join(p.hungCalls, ", ")
}

// release lets every hung call return. It is idempotent so a scenario can call
// it and defer it: the deferred call frees a hung goroutine when a failed
// assertion ends the test early, which would otherwise deadlock the bubble.
func (p *postStartProvider) release() {
	p.releaseOnce.Do(func() { close(p.releaseC) })
}

// beginAttempt readies the provider for another start of name: the observation
// ordinal restarts, and the runtime an earlier attempt created is gone, so the
// attempt starts it fresh instead of finding it running.
func (p *postStartProvider) beginAttempt(t *testing.T, name string) {
	t.Helper()
	p.observations.Store(0)
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop(%q) between attempts: %v", name, err)
	}
}

// ObserveLivenessWithError is the observation every liveness check on the start
// path goes through, and the call that hangs on the hangObservation ordinal.
func (p *postStartProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) { //nolint:unparam // runtime.LivenessObserverWithError fixes the signature; the fake observation never fails
	if ordinal := p.observations.Add(1); ordinal == p.hangObservation.Load() {
		p.hang(fmt.Sprintf("ObserveLivenessWithError #%d", ordinal))
	}
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

func (p *postStartProvider) Peek(name string, lines int) (string, error) {
	if d := time.Duration(p.peekDelay.Load()); d > 0 {
		<-time.After(d)
	}
	if p.hangPeek.Load() {
		p.hang("Peek")
	}
	return p.Fake.Peek(name, lines)
}

func (p *postStartProvider) GetMeta(name, key string) (string, error) {
	if p.hangGetMeta.Load() {
		p.hang("GetMeta " + key)
	}
	return p.Fake.GetMeta(name, key)
}

func (p *postStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.startUntilDeadline.Load() {
		<-ctx.Done()
		return fmt.Errorf("start interrupted: %w", runtime.ErrSessionExists)
	}
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
		if value := cfg.Env[key]; value != "" {
			if err := p.SetMeta(name, key, value); err != nil {
				return err
			}
		}
	}
	return p.startErr
}

// releaseAndSettle lets every hung provider call return and waits for the
// goroutines holding them to finish, the way a wedged tmux call returns long
// after the start path stopped waiting for it.
func releaseAndSettle(t *testing.T, sp *postStartProvider) {
	t.Helper()
	sp.release()
	synctest.Wait()
	if got := sp.hanging.Load(); got != 0 {
		t.Fatalf("%d provider calls still hung after release", got)
	}
}

func postStartItem(info sessionpkg.Info, workDir string) preparedStart {
	return preparedStart{
		candidate: startCandidate{
			info: info,
			tp: TemplateParams{
				Command:      "claude --resume resume-key",
				SessionName:  "worker",
				TemplateName: "worker",
			},
		},
		cfg: runtime.Config{Command: "claude --resume resume-key", WorkDir: workDir},
	}
}

// postStartBeadItem creates the pending-create session fixture in a fresh store
// and returns the prepared start for it with the bead's starting metadata.
func postStartBeadItem(t *testing.T, clk clock.Clock, workDir string) (beads.Store, beads.Bead, beads.StringMap, preparedStart) {
	t.Helper()
	store := beads.NewMemStore()
	bead, before := createStartRecoveryFixture(t, store, clk, workDir)
	return store, bead, before, postStartItem(sessiontest.SeedBead(t, bead), workDir)
}

// runPostStartAttempt runs one start attempt on its own goroutine and fails the
// test if it has not returned within postStartWatchdog of fake time. It returns
// the result and the fake time the attempt took.
func runPostStartAttempt(ctx context.Context, t *testing.T, item preparedStart, sp *postStartProvider, store beads.Store, startupTimeout time.Duration) (startResult, time.Duration) {
	t.Helper()
	done := make(chan startResult, 1)
	begin := time.Now()
	go func() {
		done <- runPreparedStartCandidate(ctx, item, "", sp, store, nil, startupTimeout, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)
	}()
	select {
	case result := <-done:
		return result, time.Since(begin)
	case <-time.After(postStartWatchdog):
		t.Fatalf("runPreparedStartCandidate had not returned %v into the attempt: a wait after provider.Start is unbounded (the bound is %v); hung provider calls: %s", postStartWatchdog, postStartBound, sp.hungIn())
		return startResult{}, 0
	}
}

// requireBeadUnchanged fails if the bead differs from want: a late answer from
// an abandoned provider call must not write anything.
func requireBeadUnchanged(t *testing.T, store beads.Store, want beads.Bead) {
	t.Helper()
	got := mustGetBead(t, store, want.ID)
	if got.Status != want.Status || !maps.Equal(got.Metadata, want.Metadata) {
		t.Fatalf("bead %s changed after the abandoned call returned: status=%q metadata=%#v, want status=%q metadata=%#v", want.ID, got.Status, got.Metadata, want.Status, want.Metadata)
	}
}

// postStartSite is one place runPreparedStartCandidate observes the runtime
// after provider.Start, wired so that observation is the one that hangs.
type postStartSite struct {
	name string
	// startErr is what the provider's Start returns once the runtime exists.
	startErr error
	// foreignRuntime starts a runtime under the session's name that belongs to
	// another incarnation, so the start collides with it instead of creating one.
	foreignRuntime bool
	// noStore runs the start without a bead store.
	noStore bool
}

var postStartSites = []postStartSite{
	{
		name:     "state sync recovery",
		startErr: fmt.Errorf("persisting post-start state: %w", sessionpkg.ErrStateSync),
	},
	{name: "session exists collision", foreignRuntime: true},
	{name: "post-stability fresh start"},
	{name: "post-stability fresh start without a store", noStore: true},
}

func startForeignRuntime(t *testing.T, sp *postStartProvider) {
	t.Helper()
	if err := sp.Fake.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start existing runtime: %v", err)
	}
	if err := sp.SetMeta("worker", "GC_SESSION_ID", "gc-previous-incarnation"); err != nil {
		t.Fatalf("SetMeta existing session ID: %v", err)
	}
	if err := sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "tok-previous"); err != nil {
		t.Fatalf("SetMeta existing instance token: %v", err)
	}
}

// storelessPostStartItem is a keyed session start with no bead store behind it,
// which takes the provider-liveness branch after startup.
func storelessPostStartItem(workDir string) preparedStart {
	return postStartItem(seedSessionInfo(beads.Bead{
		ID: "gc-worker",
		Metadata: map[string]string{
			"session_name": "worker",
			"session_key":  "resume-key",
			"template":     "worker",
		},
	}), workDir)
}

func TestRunPreparedStartCandidateDefersWhenAPostStartObservationHangs(t *testing.T) {
	for _, site := range postStartSites {
		t.Run(site.name, func(t *testing.T) {
			workDir := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
				sp := newPostStartProvider()
				sp.startErr = site.startErr
				// Observation 1 is the preflight; 2 is the one under test.
				sp.hangObservation.Store(2)
				defer sp.release()
				var store beads.Store
				var bead beads.Bead
				var before beads.StringMap
				var item preparedStart
				if site.noStore {
					item = storelessPostStartItem(workDir)
				} else {
					store, bead, before, item = postStartBeadItem(t, clk, workDir)
				}
				if site.foreignRuntime {
					startForeignRuntime(t, sp)
				}

				result, elapsed := runPostStartAttempt(context.Background(), t, item, sp, store, postStartTimeout)

				if got := sp.hangs.Load(); got != 1 {
					t.Fatalf("%d provider calls hung, want exactly the one observation under test: the scenario no longer reaches it", got)
				}
				if elapsed > postStartBound {
					t.Fatalf("the attempt took %v, want it back within the %v bound", elapsed, postStartBound)
				}
				if got := sp.CountCalls("Start", "worker"); got != 1 {
					t.Fatalf("Start calls = %d, want 1: the one that reached the runtime, or the runtime already there", got)
				}
				if got := sp.CountCalls("Stop", "worker"); got != 0 {
					t.Fatalf("Stop calls = %d, want 0 while the runtime cannot be observed", got)
				}
				if site.noStore {
					if result.err != nil || result.outcome != TraceOutcomeDeferred || result.rollbackPending || result.rateLimitScreen || result.unattributedErr != nil {
						t.Fatalf("result = (err=%v, outcome=%q, rollback=%v, rateLimit=%v, unattributed=%v), want a plain deferral", result.err, result.outcome, result.rollbackPending, result.rateLimitScreen, result.unattributedErr)
					}
				} else {
					assertUnavailableStartRecoveryDeferred(t, store, clk, bead.ID, before, result)
				}

				// The abandoned call returns late. Nothing is waiting for it, so
				// it must not panic and must not report anything.
				var settled beads.Bead
				if store != nil {
					settled = mustGetBead(t, store, bead.ID)
				}
				releaseAndSettle(t, sp)
				if store != nil {
					requireBeadUnchanged(t, store, settled)
				}
			})
		})
	}
}

func TestRunPreparedStartCandidateUsesAnObservationAnswerJustBeforeTheBound(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		store, _, _, item := postStartBeadItem(t, clk, workDir)
		sp := newPostStartProvider()
		sp.startErr = fmt.Errorf("persisting post-start state: %w", sessionpkg.ErrStateSync)
		sp.hangObservation.Store(2)
		defer sp.release()
		go func() {
			<-time.After(postStartBound - time.Millisecond)
			sp.release()
		}()

		// startup_timeout is shorter than the answer's delay: the bound on a
		// post-start observation comes from the controller's context, not from
		// the start's own deadline.
		result, elapsed := runPostStartAttempt(context.Background(), t, item, sp, store, 3*time.Second)

		if elapsed < postStartBound-time.Millisecond {
			t.Fatalf("the attempt took %v, want it to wait for the answer that arrives at %v", elapsed, postStartBound-time.Millisecond)
		}
		if result.err != nil || result.outcome != TraceOutcomeSuccess {
			t.Fatalf("result = (err=%v, outcome=%q), want success: the answer beat the bound and must be used, not reported unavailable", result.err, result.outcome)
		}
	})
}

// A start whose own deadline has expired has no time left to look at the
// runtime it collided with: the collision observation is skipped and the
// outcome is the deadline, not a deferral. Bounding the observation must not
// bring it back.
func TestRunPreparedStartCandidateSkipsTheCollisionObservationAfterTheStartDeadline(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		store, _, _, item := postStartBeadItem(t, clk, workDir)
		sp := newPostStartProvider()
		sp.startUntilDeadline.Store(true)
		sp.hangObservation.Store(2)
		defer sp.release()

		result, _ := runPostStartAttempt(context.Background(), t, item, sp, store, 5*time.Second)

		if got := sp.hangs.Load(); got != 0 {
			t.Fatalf("%d provider calls hung, want none: no post-start observation runs once the start's own deadline has expired", got)
		}
		if result.outcome != TraceOutcomeDeadlineExceeded {
			t.Fatalf("result outcome = %q (err=%v), want %q", result.outcome, result.err, TraceOutcomeDeadlineExceeded)
		}
	})
}

// lateFailureProvider returns a provider whose Start fails after the runtime
// exists and carries its identity, which sends the start down the failure-path
// helpers (the rate-limit screen peek, then the identity reads).
func lateFailureProvider() *postStartProvider {
	sp := newPostStartProvider()
	sp.startErr = errors.New("provider start failed")
	return sp
}

// assertUnattributedDeferral fails unless the result is the deferral on unknown
// attribution: no converge, no rollback, no rate-limit hold.
func assertUnattributedDeferral(t *testing.T, store beads.Store, beadID string, result startResult) {
	t.Helper()
	if result.err != nil {
		t.Fatalf("result err = %v, want nil: the start error is set aside on a deferral", result.err)
	}
	if result.outcome != TraceOutcomeDeferred {
		t.Fatalf("result outcome = %q, want %q: the runtime could not be attributed", result.outcome, TraceOutcomeDeferred)
	}
	if result.rollbackPending || result.rateLimitScreen {
		t.Fatalf("deferral carries destructive flags: rollback=%v rate-limit=%v", result.rollbackPending, result.rateLimitScreen)
	}
	if result.unattributedErr == nil || !strings.Contains(result.unattributedErr.Error(), "attribution_unknown") {
		t.Fatalf("unattributedErr = %v, want the start error annotated attribution_unknown", result.unattributedErr)
	}
	got := mustGetBead(t, store, beadID)
	if got.Status != "open" || got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("session after deferral: status=%q pending_create_claim=%q, want it left open and still claimed", got.Status, got.Metadata["pending_create_claim"])
	}
}

func TestRunPreparedStartCandidateBoundsTheRateLimitScreenPeek(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		store, bead, _, item := postStartBeadItem(t, clk, workDir)
		sp := lateFailureProvider()
		sp.hangPeek.Store(true)
		defer sp.release()

		result, elapsed := runPostStartAttempt(context.Background(), t, item, sp, store, postStartTimeout)

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung, want exactly the peek under test: the scenario no longer reaches it", got)
		}
		if elapsed > postStartBound {
			t.Fatalf("the attempt took %v, want it back within the %v budget the failure-path helpers share", elapsed, postStartBound)
		}
		assertUnattributedDeferral(t, store, bead.ID, result)
		if got := sp.CountCalls("Stop", "worker"); got != 0 {
			t.Fatalf("Stop calls = %d, want 0: a deferral on unknown attribution spares the runtime", got)
		}
		releaseAndSettle(t, sp)
	})
}

func TestRunPreparedStartCandidateBoundsThePendingCreateIdentityReads(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		store, bead, _, item := postStartBeadItem(t, clk, workDir)
		sp := lateFailureProvider()
		sp.hangGetMeta.Store(true)
		defer sp.release()

		result, elapsed := runPostStartAttempt(context.Background(), t, item, sp, store, postStartTimeout)

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung, want exactly the identity read under test: the scenario no longer reaches it", got)
		}
		if elapsed > postStartBound {
			t.Fatalf("the attempt took %v, want it back within the %v budget the failure-path helpers share", elapsed, postStartBound)
		}
		assertUnattributedDeferral(t, store, bead.ID, result)
		if got := sp.CountCalls("Stop", "worker"); got != 0 {
			t.Fatalf("Stop calls = %d, want 0: a deferral on unknown attribution spares the runtime", got)
		}
		releaseAndSettle(t, sp)
	})
}

// The rate-limit peek and the identity reads share one budget: a peek that
// takes 6 seconds leaves the identity reads 4, not another 10.
func TestRunPreparedStartCandidateFailureHelpersShareOneBudget(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		store, bead, _, item := postStartBeadItem(t, clk, workDir)
		sp := lateFailureProvider()
		sp.peekDelay.Store(int64(6 * time.Second))
		sp.hangGetMeta.Store(true)
		defer sp.release()

		result, elapsed := runPostStartAttempt(context.Background(), t, item, sp, store, postStartTimeout)

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung, want exactly the identity read under test: the scenario no longer reaches it", got)
		}
		if elapsed > postStartBound {
			t.Fatalf("the attempt took %v, want the slow peek and the hung identity read together within one %v budget", elapsed, postStartBound)
		}
		assertUnattributedDeferral(t, store, bead.ID, result)
		releaseAndSettle(t, sp)
	})
}

// A wedged session is retried every reconcile tick. Without a cap each tick
// would abandon one more provider call; the contract is at most one per session.
func TestRunPreparedStartCandidateLeavesOneAbandonedObservationPerSession(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		sp := newPostStartProvider()
		sp.hangObservation.Store(2)
		defer sp.release()
		const attempts = 100
		begin := time.Now()

		for i := 0; i < attempts; i++ {
			// A fresh bead each attempt: this test is about the provider calls,
			// not about what an earlier attempt left on the row.
			store, _, _, item := postStartBeadItem(t, clk, workDir)
			sp.beginAttempt(t, "worker")
			result, _ := runPostStartAttempt(context.Background(), t, item, sp, store, postStartTimeout)
			if result.err != nil || result.outcome != TraceOutcomeDeferred {
				t.Fatalf("attempt %d: result = (err=%v, outcome=%q), want a deferred start", i, result.err, result.outcome)
			}
		}

		if got := sp.hangs.Load(); got != 1 {
			t.Fatalf("%d provider calls hung over %d attempts, want 1: attempts that find an abandoned call outstanding must not start another", got, attempts)
		}
		if got := sp.peakHanging.Load(); got > 1 {
			t.Fatalf("%d abandoned provider calls hung at once, want at most 1", got)
		}
		if elapsed := time.Since(begin); elapsed > postStartBound {
			t.Fatalf("%d attempts took %v of fake time, want only the first to wait (at most %v)", attempts, elapsed, postStartBound)
		}

		// Once the abandoned call returns the session can be observed again.
		releaseAndSettle(t, sp)
		sp.hangObservation.Store(0)
		store, _, _, item := postStartBeadItem(t, clk, workDir)
		sp.beginAttempt(t, "worker")
		result, _ := runPostStartAttempt(context.Background(), t, item, sp, store, postStartTimeout)
		if result.err != nil || result.outcome != TraceOutcomeSuccess {
			t.Fatalf("attempt after the abandoned call returned: result = (err=%v, outcome=%q), want success", result.err, result.outcome)
		}
	})
}

// Two cities run sessions of the same name. One city's wedged runtime must not
// make the other city's start defer, so the limit of one observation in flight
// is per session per city.
func TestObserveSessionBoundedLimitsOutstandingObservationsPerCity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const cityA, cityB = "/cities/a", "/cities/b"
		wedged := make(chan struct{})
		defer close(wedged)
		observeIn := func(city string, observe func() (int, error)) (int, error) {
			ctx, cancel := context.WithTimeout(context.Background(), postStartBound)
			defer cancel()
			return observeSessionBounded(ctx, city, "worker", observe)
		}
		if _, err := observeIn(cityA, func() (int, error) { <-wedged; return 1, nil }); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("wedged observation: err = %v, want it to wrap runtime.ErrRuntimeUnavailable", err)
		}

		var ranInA atomic.Int64
		_, errA := observeIn(cityA, func() (int, error) { ranInA.Add(1); return 2, nil })
		gotB, errB := observeIn(cityB, func() (int, error) { return 3, nil })

		if got := ranInA.Load(); got != 0 || !errors.Is(errA, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("second observation of the wedged session: ran %d times, err = %v, want it declined as unavailable without running", got, errA)
		}
		if errB != nil || gotB != 3 {
			t.Fatalf("same session name in another city = (%d, %v), want (3, nil): one city's wedged runtime must not defer another's", gotB, errB)
		}
	})
}

// The async start goroutine holds its asyncStartLimiter slot until the commit
// finishes, so a start that waits forever on a provider call holds it forever.
func TestEnqueuePreparedStartWaveReleasesItsSlotWhenAPostStartObservationHangs(t *testing.T) {
	workDir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
		store, bead, before, item := postStartBeadItem(t, clk, workDir)
		sp := newPostStartProvider()
		sp.hangObservation.Store(2)
		defer sp.release()
		limiter := newAsyncStartLimiter(1)
		release, ok, reason := reserveAsyncStartSlot(context.Background(), limiter)
		if !ok {
			t.Fatalf("reserve async start slot: %s", reason)
		}
		var released, finished atomic.Int64
		finishedC := make(chan struct{})
		rec := events.NewFake()
		begin := time.Now()

		enqueuePreparedStartWaveForCity(
			context.Background(),
			[]asyncPreparedStart{{
				item: item,
				release: func() {
					released.Add(1)
					release()
				},
				done: func() {
					finished.Add(1)
					close(finishedC)
				},
			}},
			"", sp, store, nil, clk, rec, postStartTimeout, 1, ioDiscard{}, ioDiscard{}, nil, nil,
			immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil,
		)
		select {
		case <-finishedC:
		case <-time.After(postStartWatchdog):
			t.Fatalf("the async start had not finished %v in and still holds its slot: a wait after provider.Start is unbounded (the bound is %v); hung provider calls: %s", postStartWatchdog, postStartBound, sp.hungIn())
		}

		if elapsed := time.Since(begin); elapsed > postStartBound {
			t.Fatalf("the async start took %v, want it finished and its slot back within the %v bound", elapsed, postStartBound)
		}
		if got := released.Load(); got != 1 {
			t.Fatalf("slot released %d times, want 1", got)
		}
		limiter.mu.Lock()
		inFlight := limiter.inFlight
		limiter.mu.Unlock()
		if inFlight != 0 {
			t.Fatalf("asyncStartLimiter has %d starts in flight, want the slot back", inFlight)
		}
		got := mustGetBead(t, store, bead.ID)
		if got.Status != "open" || got.Metadata["pending_create_claim"] != before["pending_create_claim"] || got.Metadata["wake_attempts"] != before["wake_attempts"] {
			t.Fatalf("session after the deferred async start: status=%q pending_create_claim=%q wake_attempts=%q, want it open, still claimed, and no wake failure recorded", got.Status, got.Metadata["pending_create_claim"], got.Metadata["wake_attempts"])
		}
		if got := sp.CountCalls("Stop", "worker"); got != 0 {
			t.Fatalf("Stop calls = %d, want 0 while the runtime cannot be observed", got)
		}

		// The abandoned call returns late: it must neither report the start a
		// second time nor release the slot a second time.
		settled := mustGetBead(t, store, bead.ID)
		releaseAndSettle(t, sp)
		if released.Load() != 1 || finished.Load() != 1 {
			t.Fatalf("after the late return: slot released %d times, start finished %d times, want 1 each", released.Load(), finished.Load())
		}
		if len(rec.Events) != 0 {
			t.Fatalf("lifecycle events = %#v, want none for a deferred start", rec.Events)
		}
		requireBeadUnchanged(t, store, settled)
	})
}
