package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workqueue"
)

// The effect executor's tests (CONTRACT C1.9, P4 spec §3.4), the shared
// shutdown deadline (P4 F15 as amended), the provider-swap wait (C4.4 step 3)
// and the per-runtime-name lock (P4 F14). Timing runs in synctest bubbles.

// blockingEffect is an effect whose Run waits for release, or for its
// context when honorCtx is set, and whose Settle reports on settled.
func blockingEffect(kind sessionEffectKind, deadline time.Time, release <-chan struct{}, honorCtx bool, settled chan<- error) sessionEffect {
	return sessionEffect{
		Kind:     kind,
		Deadline: deadline,
		Run: func(ctx context.Context) error {
			if honorCtx {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			}
			<-release
			return nil
		},
		Settle: func(err error) { settled <- err },
	}
}

// Kills: a completion that does not re-run its key (or waits behind the
// key's backoff), a completion that does not wake the allocator (C1.9,
// C5.12, MAINT-054), and a failed effect re-run urgently rather than behind a
// backoff (C1.4b).
func TestEffectExecutorEnqueuesKeyUrgentAndAllocator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "a")
		h.boot(t)
		k := rowKey{Leg: routerTestLeg, ID: "a"}
		// Back the key off: its next non-urgent run waits out the backoff.
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) { return 0, errors.New("boom") })
		h.add("a", workqueue.Reason{Kind: "event"})
		synctest.Wait()
		h.rec.setSession(nil)
		before, passes := len(h.rec.callsFor("a")), len(h.rec.allocatorPasses())

		release, settled := make(chan struct{}), make(chan error, 1)
		if err := h.rt.exec.submit(k, blockingEffect(effectStart, time.Now().Add(time.Hour), release, false, settled)); err != nil {
			t.Fatal(err)
		}
		close(release)
		advance(v2AllocatorMinGap)
		if err := <-settled; err != nil {
			t.Fatalf("settled with %v", err)
		}
		calls := h.rec.callsFor("a")
		if len(calls) != before+1 || !slices.Contains(calls[len(calls)-1].kinds, v2ReasonEffect) {
			t.Fatalf("calls %+v, want one more reconcile of a, for the effect, inside its backoff", calls)
		}
		var woke bool
		for _, p := range h.rec.allocatorPasses()[passes:] {
			woke = woke || slices.Contains(p, v2ReasonEffect)
		}
		if !woke {
			t.Fatalf("allocator passes %v, want one woken by the effect", h.rec.allocatorPasses()[passes:])
		}

		// A failed effect backs its key off: no reconcile inside the 500ms
		// (±10%) base backoff, one at its end, counted as a failure.
		before = len(h.rec.callsFor("a"))
		failed := sessionEffect{Kind: effectStart, Deadline: time.Now().Add(time.Hour), Run: func(context.Context) error { return errors.New("start failed") }, Settle: func(error) {}}
		if err := h.rt.exec.submit(k, failed); err != nil {
			t.Fatal(err)
		}
		advance(400 * time.Millisecond)
		if got := len(h.rec.callsFor("a")); got != before {
			t.Fatalf("a failed effect re-ran its key inside the backoff: %+v", h.rec.callsFor("a")[before:])
		}
		advance(200 * time.Millisecond)
		calls = h.rec.callsFor("a")
		if len(calls) != before+1 || calls[before].failures != 1 || !slices.Contains(calls[before].kinds, v2ReasonEffect) {
			t.Fatalf("calls %+v, want one reconcile at the backoff's end, for the effect, after one failure", calls[before:])
		}
	})
}

// Kills: an effect stuck forever behind a hung provider call: the deadline
// must settle it (failed) and free the key.
func TestEffectExecutorDeadlineSettlesHungEffect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settled := make(chan error, 2)
		k := rowKey{Leg: routerTestLeg, ID: "a"}
		x := newEffectExecutor(func(rowKey, error) {}, io.Discard)
		hang := make(chan struct{})
		defer close(hang)
		e := sessionEffect{
			Kind:     effectStart,
			Deadline: time.Now().Add(time.Minute),
			Run:      func(context.Context) error { <-hang; return nil }, // ignores its context
			Settle:   func(err error) { settled <- err },
		}
		if err := x.submit(k, e); err != nil {
			t.Fatal(err)
		}
		if err := x.submit(k, e); !errors.Is(err, errEffectBusy) {
			t.Fatalf("second effect for the key: %v, want busy", err)
		}
		advance(time.Minute)
		if len(settled) != 1 || <-settled == nil {
			t.Fatal("at the deadline: want exactly one failed settlement")
		}
		if _, busy := x.inFlight(k); busy {
			t.Fatal("the key is still in flight after its deadline")
		}
	})
}

// Kills: two v2 budgets (the executor waiting its own full timeout after
// the workers spent theirs, P4 F15), effects leaked past the deadline, and
// executor admission still open while stop joins the workers (C1.8).
func TestStopJoinsWorkersAndExecutorWithinOneShutdownBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newV2HarnessWithRows(t, "a")
		h.boot(t)
		stuck := make(chan struct{})
		defer close(stuck)
		settled, late := make(chan error, 2), make(chan error, 1)
		h.rec.setSession(func(workqueue.Item[rowKey], *reconcileEnv) (time.Duration, error) {
			<-time.After(3 * time.Second) // a reconcile that ends 3s into the stop
			late <- h.rt.exec.submit(rowKey{Leg: routerTestLeg, ID: "a"}, blockingEffect(effectStart, time.Now().Add(time.Hour), stuck, true, settled))
			return 0, nil
		})
		h.add("a", workqueue.Reason{Kind: "event"})
		synctest.Wait()
		if err := h.rt.exec.submit(rowKey{Leg: routerTestLeg, ID: "b"}, blockingEffect(effectStart, time.Now().Add(time.Hour), stuck, true, settled)); err != nil {
			t.Fatal(err)
		}
		budget := h.rt.env.Load().shutdownTimeout()
		start := time.Now()
		h.rt.stop()
		if took := time.Since(start); took != budget {
			t.Fatalf("stop took %s, want exactly the one %s budget", took, budget)
		}
		if err := <-late; !errors.Is(err, errEffectsClosed) {
			t.Fatalf("a worker's submit during stop: %v, want closed before the workers are joined", err)
		}
		if err := <-settled; !errors.Is(err, context.Canceled) {
			t.Fatalf("the stuck effect settled with %v, want canceled at the deadline", err)
		}
		if err := h.rt.exec.submit(rowKey{Leg: routerTestLeg, ID: "c"}, blockingEffect(effectOther, time.Now().Add(time.Hour), stuck, true, settled)); !errors.Is(err, errEffectsClosed) {
			t.Fatalf("submit after stop: %v, want closed", err)
		}
	})
}

// Kills: a city shutdown that spends only what v2's stop left of one budget
// (P4 F15 as amended): a v2 start still in flight at stop uses the whole
// budget, and the agents then lose the graceful Ctrl-C pass legacy gives
// them.
func TestCityShutdownKeepsGracefulStopAfterV2StopWithStartInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := newDefaultPlanner(io.Discard)
		stuck := make(chan struct{})
		defer close(stuck)
		if err := rt.exec.submit(rowKey{Leg: routerTestLeg, ID: "a"}, blockingEffect(effectStart, time.Now().Add(time.Hour), stuck, true, make(chan error, 1))); err != nil {
			t.Fatal(err)
		}
		cfg := &config.City{}
		cfg.Daemon.ShutdownTimeout = "1s"
		sp := &interruptStopsProvider{Fake: runtime.NewFake()}
		if err := sp.Start(context.Background(), "probe", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		var stdout bytes.Buffer
		cr := &CityRuntime{cfg: cfg, sp: sp, v2: rt, rec: events.Discard, logPrefix: "gc start", stdout: &stdout, stderr: io.Discard}
		rt.stop() // run() stops v2 first, and the start holds it to the deadline
		cr.shutdown()
		if sp.CountCalls("Interrupt", "probe") != 1 || !bytes.Contains(stdout.Bytes(), []byte("waiting 1s")) {
			t.Fatalf("interrupts=%d stdout=%q, want the graceful pass with its full 1s budget", sp.CountCalls("Interrupt", "probe"), stdout.String())
		}
	})
}

// Kills: a panic in Run or Settle crashing the process (one bad effect must
// not), a Run panic logged with no stack, a panicked effect settled as a
// success or never freeing its key, and done called before the in-flight
// entry is cleared.
func TestEffectExecutorRecoversRunAndSettlePanics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stderr bytes.Buffer
		done := make(chan error, 2)
		x := newEffectExecutor(func(_ rowKey, err error) { done <- err }, &stderr)
		k := rowKey{Leg: routerTestLeg, ID: "a"}
		var settledWith error
		if err := x.submit(k, sessionEffect{
			Kind: effectStart, Deadline: time.Now().Add(time.Minute),
			Run:    func(context.Context) error { panic("provider exploded") },
			Settle: func(err error) { settledWith = err },
		}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil || settledWith == nil || err.Error() != settledWith.Error() {
			t.Fatalf("done(%v) after Settle(%v), want the panic as the error to both", err, settledWith)
		}
		if out := stderr.String(); !strings.Contains(out, "provider exploded") || !strings.Contains(out, "goroutine ") {
			t.Fatalf("stderr %q, want the panic logged with its stack", out)
		}
		if _, busy := x.inFlight(k); busy {
			t.Fatal("a panicked effect still holds its key")
		}

		if err := x.submit(k, sessionEffect{
			Kind: effectOther, Deadline: time.Now().Add(time.Minute),
			Run:    func(context.Context) error { return nil },
			Settle: func(error) { panic("ledger exploded") },
		}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("done(%v), want Run's nil after a Settle panic", err)
		}
		if !strings.Contains(stderr.String(), "ledger exploded") {
			t.Fatalf("stderr %q, want the Settle panic logged", stderr.String())
		}
	})
}

// Kills: done (the urgent enqueue) running before the in-flight entry is
// cleared, so the re-run key still reads its effect as in flight and decides
// read-only until the deadline (C5.5), over the real controller and executor.
func TestEffectDoneReRunsKeyWithEffectCleared(t *testing.T) {
	f := newSessionCtlFixture(t, sessBead("e", map[string]string{"held_until": rfc(time.Now().Add(-time.Minute))}))
	k := rowKey{Leg: routerTestLeg, ID: "e"}
	enqueue := f.exec.done
	reran := make(chan string, 1)
	f.exec.done = func(k rowKey, err error) {
		if _, err := f.reconcile(t, k.ID); err != nil {
			t.Error(err)
		}
		reran <- f.lastDecision().Reason
		enqueue(k, err)
	}
	if err := f.exec.submit(k, sessionEffect{Kind: effectStart, Deadline: time.Now().Add(time.Hour), Run: func(context.Context) error { return nil }, Settle: func(error) {}}); err != nil {
		t.Fatal(err)
	}
	if reason := <-reran; reason == decideEffectInFlight {
		t.Fatalf("the key re-ran on its effect's completion and decided %q", reason)
	}
	if f.store.writes() != 1 {
		t.Fatalf("writes = %d, want the re-run's hold clear", f.store.writes())
	}
}

// Kills: a provider swap listing the old provider while a start is still
// creating its runtime (C4.4 step 3, R6), an unbounded wait on a hung start,
// and waiting for effects that are not starts.
func TestBeforeProviderSwapWaitsInFlightStartsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newEffectExecutor(func(rowKey, error) {}, io.Discard)
		release, settled, hung := make(chan struct{}), make(chan error, 3), make(chan struct{})
		defer close(hung)
		far := time.Now().Add(time.Hour)
		must := func(err error) {
			if err != nil {
				t.Fatal(err)
			}
		}
		must(x.submit(rowKey{ID: "start"}, blockingEffect(effectStart, far, release, false, settled)))
		must(x.submit(rowKey{ID: "stop"}, blockingEffect(effectStop, far, hung, false, settled)))
		go func() {
			<-time.After(20 * time.Second)
			close(release)
		}()
		start := time.Now()
		if err := x.waitStarts(time.Minute); err != nil || time.Since(start) != 20*time.Second {
			t.Fatalf("waitStarts = %v after %s, want nil once the start settled at 20s", err, time.Since(start))
		}
		<-settled

		must(x.submit(rowKey{ID: "hung"}, blockingEffect(effectStart, far, hung, false, settled)))
		start = time.Now()
		if err := x.waitStarts(time.Minute); err == nil || time.Since(start) != 70*time.Second {
			t.Fatalf("hung start: %v after %s, want an error at the bound plus the cancel bound", err, time.Since(start))
		}

		x = newEffectExecutor(func(rowKey, error) {}, io.Discard) // the hung start above still runs
		must(x.submit(rowKey{ID: "cancelable"}, blockingEffect(effectStart, far, hung, true, settled)))
		start = time.Now()
		if err := x.waitStarts(time.Minute); err != nil || time.Since(start) != time.Minute {
			t.Fatalf("cancelable start: %v after %s, want it canceled and settled at the bound", err, time.Since(start))
		}
	})
}

// Kills: a reaper stopping the fresh runtime a start put under a closed
// row's runtime name (P4 F14): it must skip a name whose lock a start holds,
// and re-read GC_SESSION_ID under the lock before its Stop.
func TestRuntimeNameLockSerializesReaperAndStart(t *testing.T) {
	closed := routerSessionBead("old", map[string]string{"session_name": "named-1"})
	closed.Status = "closed"
	fresh := routerSessionBead("new", map[string]string{"session_name": "named-1"})
	setup := func(t *testing.T) (*runtime.Fake, *reapRaceStore) {
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "named-1", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if err := sp.SetMeta("named-1", "GC_SESSION_ID", "old"); err != nil {
			t.Fatal(err)
		}
		return sp, &reapRaceStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{closed, fresh}, nil)}
	}
	freshInfo, err := sessionFrontDoor(beads.NewMemStoreFrom(0, []beads.Bead{fresh}, nil)).Get("new")
	if err != nil {
		t.Fatal(err)
	}
	open := newSessionBeadSnapshotFromInfos([]session.Info{freshInfo})

	sp, store := setup(t)
	unlock := runtimeNames.tryLock("city-a", "named-1") // a start is mid provider Start
	if n := reapRuntimesBoundToClosedBeads(store, open, nil, sp, nil, "city-a", io.Discard); n != 0 || sp.CountCalls("Stop", "named-1") != 0 {
		t.Fatal("the reaper stopped a name a start holds")
	}
	if n := reapRuntimesBoundToClosedBeads(store, open, nil, sp, nil, "city-b", io.Discard); n != 1 {
		t.Fatal("another city's start on the same runtime name blocked the reap")
	}
	unlock()

	sp, store = setup(t)
	store.onLiveGet = func() { // the start lands while the reaper reads the store
		_ = sp.SetMeta("named-1", "GC_SESSION_ID", "new") //nolint:errcheck // fake
	}
	var stderr bytes.Buffer
	if n := reapRuntimesBoundToClosedBeads(store, open, nil, sp, nil, "city-a", &stderr); n != 0 || sp.CountCalls("Stop", "named-1") != 0 {
		t.Fatalf("the reaper stopped the fresh runtime: %s", stderr.String())
	}

	sp, store = setup(t)
	if n := reapRuntimesBoundToClosedBeads(store, open, nil, sp, nil, "city-a", io.Discard); n != 1 {
		t.Fatal("the reaper must still reap a runtime bound to a closed bead")
	}
}

// Kills: a reaper path that leaves the runtime-name lock held (the
// lane-gone path's unlock, MAINT-032), so every later start and reap of the
// name is skipped forever, and a provider panic that leaks it.
func TestReaperFreesRuntimeNameLockOnEveryPath(t *testing.T) {
	for _, c := range []struct {
		name    string
		sp      func() runtime.Provider
		stopped bool
	}{
		{"binding changed", func() runtime.Provider { return boundFake(t, "fresh") }, false},
		{"lane-nominated name gone", func() runtime.Provider { return &unlistedProvider{Fake: boundFake(t, "old")} }, false},
		{"stopped", func() runtime.Provider { return boundFake(t, "old") }, true},
		{"stop panicked", func() runtime.Provider { return &panickingStopProvider{Fake: boundFake(t, "old")} }, false},
	} {
		func() {
			defer func() { _ = recover() }()
			if stopped, _ := stopStillBoundClosedRuntime("city-a", "named-1", "old", c.sp(), true); stopped != c.stopped {
				t.Errorf("%s: stopped=%v, want %v", c.name, stopped, c.stopped)
			}
		}()
		unlock := runtimeNames.tryLock("city-a", "named-1")
		if unlock == nil {
			t.Fatalf("%s: the name lock is still held", c.name)
		}
		unlock()
	}
}

// boundFake runs named-1 bound to session id.
func boundFake(t *testing.T, id string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "named-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetMeta("named-1", "GC_SESSION_ID", id); err != nil {
		t.Fatal(err)
	}
	return sp
}

// unlistedProvider's exact-name listing no longer shows any name.
type unlistedProvider struct{ *runtime.Fake }

func (*unlistedProvider) ListRunning(string) ([]string, error) { return nil, nil }

// panickingStopProvider panics in Stop.
type panickingStopProvider struct{ *runtime.Fake }

func (*panickingStopProvider) Stop(string) error { panic("stop exploded") }

// reapRaceStore runs onLiveGet before each live Get.
type reapRaceStore struct {
	beads.Store
	onLiveGet func()
}

func (s *reapRaceStore) Get(id string) (beads.Bead, error) {
	if s.onLiveGet != nil {
		s.onLiveGet()
	}
	return s.Store.Get(id)
}

// Kills: the reload's provider swap listing the old provider's sessions
// while a v2 start may still create one (the call to beforeProviderSwap
// missing from reloadConfigTraced, C4.4 step 3); and the planner's starts
// left running through the wait, or left paused once the reload aborts
// (CONTRACT v5 P7).
func TestReloadProviderSwapWaitsForV2StartsBeforeListing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cityPath := t.TempDir()
		tomlPath := filepath.Join(cityPath, "city.toml")
		writeCityRuntimeConfig(t, tomlPath, "fake")
		cfg, err := config.Load(osFS{}, tomlPath)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		sp := &swapListRecorder{Fake: runtime.NewFake()}
		cr := newTestCityRuntime(t, CityRuntimeParams{
			CityPath: cityPath, CityName: "test-city", TomlPath: tomlPath, Cfg: cfg, SP: sp,
			BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
				return DesiredStateResult{State: map[string]TemplateParams{}}
			},
			Dops: newDrainOps(sp), Rec: events.Discard, Stdout: io.Discard, Stderr: io.Discard,
		})
		cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
		cs.cityBeadStore = beads.NewMemStore()
		cr.setControllerState(cs)
		cr.sessionDrains = newDrainTracker()
		cr.v2 = newDefaultPlanner(io.Discard)
		hung := make(chan struct{})
		defer close(hung)
		var pausedAtCancel atomic.Bool
		if err := cr.v2.exec.submit(rowKey{ID: "s"}, sessionEffect{
			Kind: effectStart, Deadline: time.Now().Add(time.Hour),
			Run: func(ctx context.Context) error {
				<-ctx.Done() // waitStarts cancels it; it hangs on regardless
				pausedAtCancel.Store(cr.v2.planner.startsPaused())
				<-hung
				return ctx.Err()
			},
			Settle: func(error) {},
		}); err != nil {
			t.Fatal(err)
		}

		writeCityRuntimeConfig(t, tomlPath, "fail")
		lastProviderName := "fake"
		reply := cr.reloadConfigTraced(context.Background(), &lastProviderName, cityPath, nil, reloadSourceManual)
		if reply.Outcome != reloadOutcomeFailed || sp.listings != 0 {
			t.Fatalf("reply %q after %d listings, want the swap refused before any listing", reply.Outcome, sp.listings)
		}
		if !pausedAtCancel.Load() || cr.v2.planner.startsPaused() {
			t.Fatalf("starts paused at the cancel = %v, after the abort = %v; want paused, then resumed", pausedAtCancel.Load(), cr.v2.planner.startsPaused())
		}
		if _, err := (&CityRuntime{}).beforeProviderSwap(cfg); err != nil {
			t.Fatalf("a legacy controller waits on nothing: %v", err)
		}
	})
}

// Kills the swap wait taken from a startup_timeout of zero (CONTRACT v5.4
// P3 reads it as the 60s default, as admit's start deadline does): a start
// that needs a minute is waited for, not canceled at 10s.
func TestBeforeProviderSwapWaitsTheDefaultStartupForZero(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cr := &CityRuntime{v2: newDefaultPlanner(io.Discard)}
		release, settled := make(chan struct{}), make(chan error, 1)
		time.AfterFunc(time.Minute, func() { close(release) })
		if err := cr.v2.exec.submit(rowKey{ID: "s"}, blockingEffect(effectStart, time.Now().Add(time.Hour), release, true, settled)); err != nil {
			t.Fatal(err)
		}
		cfg := &config.City{Session: config.SessionConfig{StartupTimeout: "0s"}}
		resume, err := cr.beforeProviderSwap(cfg)
		resume()
		if err != nil {
			t.Fatalf("beforeProviderSwap = %v, want the minute-long start waited for", err)
		}
		if err := <-settled; err != nil {
			t.Fatalf("the start settled %v, want it to finish uncanceled", err)
		}
	})
}

// swapListRecorder counts ListRunning calls.
type swapListRecorder struct {
	*runtime.Fake
	listings int
}

func (p *swapListRecorder) ListRunning(prefix string) ([]string, error) {
	p.listings++
	return p.Fake.ListRunning(prefix)
}
