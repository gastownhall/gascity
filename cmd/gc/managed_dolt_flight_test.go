package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The managed-Dolt preflight used to run the provider health ladder, and with it
// a recover that can take minutes, to completion on whichever goroutine called
// it (the tick, the startup pass, the control dispatcher or the orders lane),
// one at a time behind a mutex. These tests pin the bounded version. A recover
// runs as a flight, one per city, under the context of the caller that started
// it; every caller waits for the flight at most managedDoltFlightWindow from
// the flight's start, then proceeds and leaves it running.
//
// Every test runs inside a testing/synctest bubble, so the window, the recover
// that outlives it and the test's own waits share one virtual clock: a receive
// on time.After advances it exactly, at no wall-clock cost, and synctest.Wait
// returns only once every other goroutine is blocked again, so each state read
// after it is settled rather than polled for. The window is written as a
// literal 30s throughout, so changing the constant fails here.

// managedDoltExpiryLog is the line a caller logs when it stops waiting for a
// recover that is still running.
const managedDoltExpiryLog = "managed dolt preflight still running after 30s"

// stuckRecover stands in for the managed-Dolt health ladder while a recover
// runs: every call blocks until finish, or until the context it was handed
// ends, and then returns the error finish was given.
type stuckRecover struct {
	calls   atomic.Int32
	release chan struct{}
	once    sync.Once
	err     error // set by finish before release closes

	mu   sync.Mutex
	ctxs []context.Context
}

func newStuckRecover() *stuckRecover {
	return &stuckRecover{release: make(chan struct{})}
}

// run is the recover as the flight registry calls it.
func (r *stuckRecover) run(ctx context.Context) error {
	r.calls.Add(1)
	r.mu.Lock()
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
	select {
	case <-r.release:
		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// health is the recover as the runtime's managedDoltHealth hook calls it.
func (r *stuckRecover) health(ctx context.Context, _ string) error {
	return r.run(ctx)
}

// finish ends the recover, and every call after it, with err.
func (r *stuckRecover) finish(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.release)
	})
}

// firstCallContext returns the context the recover's first call was handed.
func (r *stuckRecover) firstCallContext(t *testing.T) context.Context {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ctxs) == 0 {
		t.Fatal("the recover was never called")
	}
	return r.ctxs[0]
}

func (r *stuckRecover) wantCalls(t *testing.T, want int32, when string) {
	t.Helper()
	if got := r.calls.Load(); got != want {
		t.Fatalf("%s: recover calls = %d, want %d", when, got, want)
	}
}

// resetManagedDoltFlightsForTest empties the flight registry now and again when
// the test ends, so a flight one test leaves running cannot be joined by the
// next.
func resetManagedDoltFlightsForTest(t *testing.T) {
	t.Helper()
	managedDoltFlights.Clear()
	t.Cleanup(managedDoltFlights.Clear)
}

// advanceFlightClock moves the bubble clock forward by d and lets every
// goroutine settle.
func advanceFlightClock(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}

// wantFlightStartedAt fails unless the city's preflight flight is registered
// and started at want. A test about to start a second concurrent caller
// asserts this first: a preflight that does not register its recover then
// fails on the registry read, instead of leaving the second caller queued on a
// lock, which a bubble cannot see through (a goroutine blocked on a mutex is
// not durably blocked, so synctest.Wait never returns).
func wantFlightStartedAt(t *testing.T, cityPath string, want time.Time) {
	t.Helper()
	got, ok := managedDoltFlightStartedAt(cityPath)
	if !ok {
		t.Fatalf("no managed-dolt flight is registered for the city, want one started at %s", want.UTC().Format(time.RFC3339Nano))
	}
	if !got.Equal(want) {
		t.Fatalf("managed-dolt flight started at %s, want %s", got.UTC().Format(time.RFC3339Nano), want.UTC().Format(time.RFC3339Nano))
	}
}

func wantNoFlight(t *testing.T, cityPath, when string) {
	t.Helper()
	if started, ok := managedDoltFlightStartedAt(cityPath); ok {
		t.Fatalf("%s: a managed-dolt flight started at %s is still registered, want none", when, started.UTC().Format(time.RFC3339Nano))
	}
}

func wantLogCount(t *testing.T, stderr *lockedBuffer, substr string, want int, when string) {
	t.Helper()
	if got := strings.Count(stderr.String(), substr); got != want {
		t.Fatalf("%s: stderr has %d lines with %q, want %d; stderr:\n%s", when, got, substr, want, stderr.String())
	}
}

// managedDoltPreflightRuntime is a runtime whose preflight is armed (the bd
// store contract, a lifecycle-owned Dolt, no published port) with health
// standing in for the provider health ladder. Only the preflight hooks are
// set, so a call under test reaches nothing else.
func managedDoltPreflightRuntime(t *testing.T, health func(context.Context, string) error, stderr io.Writer) *CityRuntime {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	resetManagedDoltFlightsForTest(t)
	return &CityRuntime{
		cityPath:          t.TempDir(),
		logPrefix:         "gc test",
		stderr:            stderr,
		managedDoltHealth: health,
		managedDoltOwned:  func(string) (bool, error) { return true, nil },
		managedDoltPort:   func(string) string { return "" },
	}
}

// startManagedDoltPreflight runs one preflight call on its own goroutine and
// returns a channel that closes when the call returns.
func startManagedDoltPreflight(ctx context.Context, cr *CityRuntime) <-chan struct{} {
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		cr.ensureManagedDoltPublishedForTick(ctx)
	}()
	return returned
}

func wantPending(t *testing.T, done <-chan struct{}, when string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("%s: returned, want still waiting", when)
	default:
	}
}

func wantReturned(t *testing.T, done <-chan struct{}, when string) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatalf("%s: still waiting, want returned", when)
	}
}

// The window is the contract ga-6jzn19 builds on, so its length is pinned.
func TestManagedDoltFlightWindowIsThirtySeconds(t *testing.T) {
	if managedDoltFlightWindow != 30*time.Second {
		t.Fatalf("managedDoltFlightWindow = %s, want 30s", managedDoltFlightWindow)
	}
}

// Never two recovers at once: however many callers arrive for a city while its
// flight is running, the flight function runs once and they all get that
// flight.
func TestManagedDoltFlightRunsOnceForConcurrentCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		probe := newStuckRecover()
		defer probe.finish(nil)

		const callers = 5
		flights := make([]*managedDoltFlight, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Go(func() {
				flights[i] = joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
			})
		}
		wg.Wait()
		synctest.Wait()

		probe.wantCalls(t, 1, "five concurrent callers")
		for i := 1; i < callers; i++ {
			if flights[i] != flights[0] {
				t.Fatalf("caller %d got a different flight than caller 0", i)
			}
		}
	})
}

// The registry is per city: one city's recover must not stand in for another's.
func TestManagedDoltFlightsAreSeparatePerCity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		cityA, cityB := t.TempDir(), t.TempDir()
		probeA, probeB := newStuckRecover(), newStuckRecover()
		defer probeA.finish(nil)
		defer probeB.finish(nil)

		a := joinOrStartManagedDoltFlight(context.Background(), cityA, probeA.run)
		b := joinOrStartManagedDoltFlight(context.Background(), cityB, probeB.run)
		synctest.Wait()

		probeA.wantCalls(t, 1, "city A")
		probeB.wantCalls(t, 1, "city B")
		if a == b {
			t.Fatal("two cities share one flight")
		}
	})
}

// The bd env builders ga-6jzn19 attaches only have a cityPath, spelled however
// their caller spelled it, so the key is the normalized path.
func TestManagedDoltFlightJoinsAcrossSpellingsOfOneCityPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		alias := city + "/elsewhere/.."
		probe := newStuckRecover()
		defer probe.finish(nil)

		first := joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
		second := joinOrStartManagedDoltFlight(context.Background(), alias, probe.run)
		synctest.Wait()

		probe.wantCalls(t, 1, "two spellings of one city path")
		if second != first {
			t.Fatal("the second spelling started a flight of its own")
		}
		if _, ok := managedDoltFlightStartedAt(alias); !ok {
			t.Fatal("the in-flight read does not see the flight under the second spelling")
		}
	})
}

// The flight belongs to the caller that started it: its context is the
// flight's, so shutdown (which ends the starter's context) cancels the recover,
// and a joiner whose own context ends cannot cancel a recover it did not start.
func TestManagedDoltFlightRunsUnderTheStartersContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		probe := newStuckRecover()
		defer probe.finish(nil)
		starterCtx, cancelStarter := context.WithCancel(context.Background())
		defer cancelStarter()
		joinerCtx, cancelJoiner := context.WithCancel(context.Background())
		defer cancelJoiner()

		flight := joinOrStartManagedDoltFlight(starterCtx, city, probe.run)
		synctest.Wait()
		probe.wantCalls(t, 1, "flight start")
		runCtx := probe.firstCallContext(t)

		if joined := joinOrStartManagedDoltFlight(joinerCtx, city, probe.run); joined != flight {
			t.Fatal("the second caller started a flight of its own")
		}
		cancelJoiner()
		synctest.Wait()
		if err := runCtx.Err(); err != nil {
			t.Fatalf("a joiner's context ending canceled the running flight: %v", err)
		}

		cancelStarter()
		synctest.Wait()
		if runCtx.Err() == nil {
			t.Fatal("the starter's context ending did not cancel the flight")
		}
		if finished, err := flight.wait(context.Background()); !finished || !errors.Is(err, context.Canceled) {
			t.Fatalf("flight.wait = (%v, %v) after the starter's context ended, want (true, context canceled)", finished, err)
		}
	})
}

// wait ends with its own context, not the flight's: the caller moves on and the
// recover keeps running, still registered and with its context live.
func TestManagedDoltFlightWaitEndsWithItsContextAndLeavesTheFlightRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		probe := newStuckRecover()
		defer probe.finish(nil)
		flight := joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
		synctest.Wait()
		probe.wantCalls(t, 1, "flight start")

		waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelWait()
		var finished bool
		var waitErr error
		waited := make(chan struct{})
		go func() {
			defer close(waited)
			finished, waitErr = flight.wait(waitCtx)
		}()
		synctest.Wait()
		wantPending(t, waited, "wait while the flight runs and its context lives")
		advanceFlightClock(10*time.Second - time.Nanosecond)
		wantPending(t, waited, "wait 1ns before its context ends")
		advanceFlightClock(time.Nanosecond)
		wantReturned(t, waited, "wait once its context ended")

		if finished || waitErr != nil {
			t.Fatalf("wait = (%v, %v) when its context ended first, want (false, nil)", finished, waitErr)
		}
		if _, ok := managedDoltFlightStartedAt(city); !ok {
			t.Fatal("the flight left the registry when a waiter's context ended")
		}
		if err := probe.firstCallContext(t).Err(); err != nil {
			t.Fatalf("a waiter's context ending canceled the flight: %v", err)
		}

		probe.finish(nil)
		synctest.Wait()
		if finished, err := flight.wait(context.Background()); !finished || err != nil {
			t.Fatalf("wait after the flight ended = (%v, %v), want (true, nil)", finished, err)
		}
	})
}

// Whoever waits, before or after the flight ends, gets the error it ended with.
func TestManagedDoltFlightDeliversItsErrorToEveryWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		probe := newStuckRecover()
		defer probe.finish(nil)
		errDown := errors.New("dolt did not come up")
		flight := joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
		synctest.Wait()
		probe.wantCalls(t, 1, "flight start")

		const waiters = 3
		finished := make([]bool, waiters)
		errs := make([]error, waiters)
		var wg sync.WaitGroup
		for i := range waiters {
			wg.Go(func() {
				finished[i], errs[i] = flight.wait(context.Background())
			})
		}
		synctest.Wait()
		probe.finish(errDown)
		wg.Wait()

		for i := range waiters {
			if !finished[i] || !errors.Is(errs[i], errDown) {
				t.Fatalf("waiter %d got (%v, %v), want (true, %v)", i, finished[i], errs[i], errDown)
			}
		}
		if ok, err := flight.wait(context.Background()); !ok || !errors.Is(err, errDown) {
			t.Fatalf("a waiter after the flight ended got (%v, %v), want (true, %v)", ok, err, errDown)
		}
	})
}

// A finished flight is gone before anyone can see it finish, so the next caller
// starts a fresh recover instead of joining a dead one.
func TestManagedDoltFlightIsRemovedWhenItFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		probe := newStuckRecover()
		defer probe.finish(nil)

		first := joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
		synctest.Wait()
		probe.wantCalls(t, 1, "first flight")
		probe.finish(nil)
		if finished, _ := first.wait(context.Background()); !finished {
			t.Fatal("the first flight did not finish")
		}
		wantNoFlight(t, city, "the instant the first flight is seen to finish")

		second := joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
		synctest.Wait()
		probe.wantCalls(t, 2, "a call after the first flight finished")
		if second == first {
			t.Fatal("a call after the first flight finished joined the finished flight")
		}
	})
}

// The non-blocking read gives the flight's start time, which stays the first
// caller's however many callers join and however long the flight runs.
func TestManagedDoltFlightStartedAtReportsTheRunningFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		probe := newStuckRecover()
		defer probe.finish(nil)

		wantNoFlight(t, city, "before any flight")
		t0 := time.Now()
		joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
		synctest.Wait()
		probe.wantCalls(t, 1, "flight start")
		wantFlightStartedAt(t, city, t0)

		advanceFlightClock(7 * time.Second)
		joinOrStartManagedDoltFlight(context.Background(), city, probe.run)
		wantFlightStartedAt(t, city, t0)

		probe.finish(nil)
		synctest.Wait()
		wantNoFlight(t, city, "after the flight finished")
	})
}

// No lock is held across the flight function: ga-6jzn19 runs a second entry
// point inside a flight, and it must be able to read and join the registry.
func TestManagedDoltFlightRunMayConsultTheRegistry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetManagedDoltFlightsForTest(t)
		city := t.TempDir()
		type view struct {
			inFlight bool
			joined   *managedDoltFlight
		}
		consulted := make(chan view, 1)

		flight := joinOrStartManagedDoltFlight(context.Background(), city, func(ctx context.Context) error {
			_, inFlight := managedDoltFlightStartedAt(city)
			joined := joinOrStartManagedDoltFlight(ctx, city, func(context.Context) error {
				return errors.New("a flight started inside a flight")
			})
			consulted <- view{inFlight: inFlight, joined: joined}
			return nil
		})
		synctest.Wait()

		select {
		case v := <-consulted:
			if !v.inFlight {
				t.Fatal("inside the flight function, the in-flight read does not see the flight")
			}
			if v.joined != flight {
				t.Fatal("a join from inside the flight function did not return the running flight")
			}
		default:
			t.Fatal("the flight function never reported back")
		}
	})
}

// The defect: one stuck recover held the tick for as long as it took. The
// preflight now returns when the window closes, to the nanosecond, with the
// recover still running and called once.
func TestManagedDoltPreflightReturnsAtTheWindowWhileTheRecoverKeepsRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		cr := managedDoltPreflightRuntime(t, probe.health, io.Discard)

		returned := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		probe.wantCalls(t, 1, "preflight start")
		wantPending(t, returned, "the preflight at t=0")

		advanceFlightClock(30*time.Second - time.Nanosecond)
		wantPending(t, returned, "the preflight 1ns before the window closes")
		advanceFlightClock(time.Nanosecond)
		wantReturned(t, returned, "the preflight when the window closes")
		probe.wantCalls(t, 1, "after the window")
	})
}

// A recover that outlives the window is logged once, not once per tick, and
// the ticks that arrive while it still runs neither wait for it nor start
// another.
func TestManagedDoltPreflightLogsTheExpiryOncePerRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		var stderr lockedBuffer
		cr := managedDoltPreflightRuntime(t, probe.health, &stderr)

		first := startManagedDoltPreflight(context.Background(), cr)
		wantLogCount(t, &stderr, managedDoltExpiryLog, 0, "before the window closes")
		advanceFlightClock(30 * time.Second)
		wantReturned(t, first, "the first tick when the window closes")
		wantLogCount(t, &stderr, managedDoltExpiryLog, 1, "the window closing")

		for tick := 2; tick <= 4; tick++ {
			advanceFlightClock(15 * time.Second)
			later := startManagedDoltPreflight(context.Background(), cr)
			synctest.Wait()
			wantReturned(t, later, "a tick while the recover is still running past the window")
		}
		probe.wantCalls(t, 1, "ticks while the first recover is still running")
		wantLogCount(t, &stderr, managedDoltExpiryLog, 1, "ticks while the first recover is still running")
	})
}

// A tick after the recover finished sees its result and does nothing: the port
// is published, so there is no health call, no new flight, no log.
func TestManagedDoltPreflightLaterTickSeesTheFinishedRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		var stderr lockedBuffer
		cr := managedDoltPreflightRuntime(t, probe.health, &stderr)
		var published atomic.Bool
		cr.managedDoltPort = func(string) string {
			if published.Load() {
				return "3307"
			}
			return ""
		}

		first := startManagedDoltPreflight(context.Background(), cr)
		advanceFlightClock(30 * time.Second)
		wantReturned(t, first, "the first tick when the window closes")

		published.Store(true)
		probe.finish(nil)
		synctest.Wait()
		wantNoFlight(t, cr.cityPath, "after the recover finished")

		later := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		wantReturned(t, later, "a tick after the recover published the port")
		probe.wantCalls(t, 1, "a tick after the recover published the port")
		wantNoFlight(t, cr.cityPath, "a tick after the recover published the port")
		wantLogCount(t, &stderr, "health preflight", 0, "a recover that succeeded")
	})
}

// The recover's error is logged by the flight, once, whoever is waiting when
// the recover ends; a recover that failed leaves the port unpublished, so the
// next tick starts a fresh one.
func TestManagedDoltPreflightLogsARecoverFailureOnceAndRetriesOnTheNextTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		var stderr lockedBuffer
		cr := managedDoltPreflightRuntime(t, probe.health, &stderr)
		const failure = "gc test: managed dolt health preflight: dolt did not come up"

		first := startManagedDoltPreflight(context.Background(), cr)
		advanceFlightClock(30 * time.Second)
		wantReturned(t, first, "the first tick when the window closes")
		second := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		wantReturned(t, second, "a tick past the window")

		probe.finish(errors.New("dolt did not come up"))
		synctest.Wait()
		wantLogCount(t, &stderr, failure, 1, "one recover, two ticks that stopped waiting for it")
		wantNoFlight(t, cr.cityPath, "after the failed recover ended")

		third := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		wantReturned(t, third, "a tick after the failed recover")
		probe.wantCalls(t, 2, "a tick after the failed recover, port still unpublished")
		wantLogCount(t, &stderr, failure, 2, "the second recover failing the same way")
	})
}

// Two callers inside the window share one recover and one failure line.
func TestManagedDoltPreflightJoinerDoesNotLogTheOwnersFailureAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		var stderr lockedBuffer
		cr := managedDoltPreflightRuntime(t, probe.health, &stderr)
		t0 := time.Now()

		owner := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		probe.wantCalls(t, 1, "owner start")
		wantFlightStartedAt(t, cr.cityPath, t0)
		advanceFlightClock(5 * time.Second)
		joiner := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		wantPending(t, owner, "the owner 5s into the recover")
		wantPending(t, joiner, "the joiner 5s into the recover")

		advanceFlightClock(5 * time.Second)
		probe.finish(errors.New("dolt did not come up"))
		synctest.Wait()
		wantReturned(t, owner, "the owner when the recover ended")
		wantReturned(t, joiner, "the joiner when the recover ended")
		probe.wantCalls(t, 1, "owner and joiner")
		wantLogCount(t, &stderr, "gc test: managed dolt health preflight: dolt did not come up", 1, "owner and joiner")
		wantLogCount(t, &stderr, managedDoltExpiryLog, 0, "a recover that ended inside the window")
	})
}

// The recover is tied to the controller's lifetime: shutdown cancels it and
// frees the caller, and that is not a window expiry, so nothing is logged as
// one.
func TestManagedDoltPreflightShutdownCancelsTheRecover(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		var stderr lockedBuffer
		cr := managedDoltPreflightRuntime(t, probe.health, &stderr)
		lifetime, shutdown := context.WithCancel(context.Background())
		defer shutdown()

		returned := startManagedDoltPreflight(lifetime, cr)
		advanceFlightClock(5 * time.Second)
		probe.wantCalls(t, 1, "preflight start")
		wantPending(t, returned, "the preflight 5s into the recover")

		shutdown()
		synctest.Wait()
		wantReturned(t, returned, "the preflight after shutdown")
		if probe.firstCallContext(t).Err() == nil {
			t.Fatal("shutdown left the recover's context live")
		}
		wantLogCount(t, &stderr, managedDoltExpiryLog, 0, "a shutdown")
		wantNoFlight(t, cr.cityPath, "after the canceled recover ended")
	})
}

// The window bounds the wait, never the recover: Dolt's start is not
// interrupted when a tick moves on, and the recover keeps its own context,
// with no deadline of the tick's.
func TestManagedDoltPreflightWindowExpiryLeavesTheRecoverRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		cr := managedDoltPreflightRuntime(t, probe.health, io.Discard)

		returned := startManagedDoltPreflight(context.Background(), cr)
		advanceFlightClock(45 * time.Second)
		probe.wantCalls(t, 1, "45s in")
		hookCtx := probe.firstCallContext(t)
		if err := hookCtx.Err(); err != nil {
			t.Fatalf("the window closing canceled the recover: %v", err)
		}
		if deadline, ok := hookCtx.Deadline(); ok {
			t.Fatalf("the recover runs under a deadline (%s); only the wait is bounded", deadline.UTC().Format(time.RFC3339Nano))
		}

		probe.finish(nil)
		synctest.Wait()
		wantReturned(t, returned, "the preflight after the recover finished")
		wantNoFlight(t, cr.cityPath, "after the recover finished")
	})
}

// The window runs from the flight's start, not from each caller's arrival: a
// caller that joins 10s in waits the remaining 20s, and one that arrives after
// the window does not wait at all, so a stream of ticks cannot keep any one of
// them waiting longer than the first.
func TestManagedDoltPreflightWindowIsAnchoredToTheRecoversStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		cr := managedDoltPreflightRuntime(t, probe.health, io.Discard)
		t0 := time.Now()

		first := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		probe.wantCalls(t, 1, "first caller")
		wantFlightStartedAt(t, cr.cityPath, t0)

		advanceFlightClock(10 * time.Second)
		joiner := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		wantPending(t, joiner, "a joiner 10s into the recover")
		advanceFlightClock(20*time.Second - time.Nanosecond)
		wantPending(t, joiner, "the joiner 1ns before the recover's window closes")
		advanceFlightClock(time.Nanosecond)
		wantReturned(t, joiner, "the joiner when the recover's window closes, 20s after it arrived")
		wantReturned(t, first, "the first caller when the window closes")

		advanceFlightClock(15 * time.Second)
		late := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		wantReturned(t, late, "a caller 45s into the recover")
		probe.wantCalls(t, 1, "first caller, joiner and late caller")
	})
}

// The orders lane runs the same preflight as the tick. While the tick's recover
// runs, a lane pass must not queue behind it: it gets the rest of the window,
// then dispatches with the recover still running, and a pass after the window
// dispatches without waiting at all.
func TestOrdersLanePassDoesNotQueueBehindARecoverPastTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probe := newStuckRecover()
		defer probe.finish(nil)
		od := &recordingOrderDispatcher{}
		cr := ordersLaneTestRuntime(t, od, "1h", nil)
		t.Setenv("GC_BEADS", "bd")
		resetManagedDoltFlightsForTest(t)
		cr.managedDoltHealth = probe.health
		cr.managedDoltOwned = func(string) (bool, error) { return true, nil }
		cr.managedDoltPort = func(string) string { return "" }
		t0 := time.Now()
		runPass := func() <-chan struct{} {
			done := make(chan struct{})
			go func() {
				defer close(done)
				cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
			}()
			return done
		}

		tick := startManagedDoltPreflight(context.Background(), cr)
		synctest.Wait()
		probe.wantCalls(t, 1, "the tick's preflight")
		wantFlightStartedAt(t, cr.cityPath, t0)

		advanceFlightClock(10 * time.Second)
		pass := runPass()
		synctest.Wait()
		wantPending(t, pass, "a lane pass 10s into the tick's recover")
		advanceFlightClock(20*time.Second - time.Nanosecond)
		wantPending(t, pass, "the lane pass 1ns before the recover's window closes")
		if got := od.calls.Load(); got != 0 {
			t.Fatalf("orders dispatched %d times before the window closed, want 0", got)
		}
		advanceFlightClock(time.Nanosecond)
		wantReturned(t, pass, "the lane pass when the recover's window closes")
		wantReturned(t, tick, "the tick's preflight when the window closes")
		if got := od.calls.Load(); got != 1 {
			t.Fatalf("orders dispatched %d times when the window closed, want 1", got)
		}

		advanceFlightClock(time.Minute)
		next := runPass()
		synctest.Wait()
		wantReturned(t, next, "a lane pass a minute past the window")
		if got := od.calls.Load(); got != 2 {
			t.Fatalf("orders dispatched %d times after the second pass, want 2", got)
		}
		probe.wantCalls(t, 1, "the tick and both lane passes")
	})
}

// The recover now runs on a goroutine of its own, where a panic would take the
// controller down instead of the one tick that used to run it. It is contained
// and logged, the flight is cleared, and the next call starts afresh.
func TestManagedDoltPreflightContainsAHealthHookPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stderr lockedBuffer
		var calls atomic.Int32
		cr := managedDoltPreflightRuntime(t, func(context.Context, string) error {
			calls.Add(1)
			panic("dolt exploded")
		}, &stderr)

		for call := 1; call <= 2; call++ {
			var leaked any
			func() {
				defer func() { leaked = recover() }()
				cr.ensureManagedDoltPublishedForTick(context.Background())
			}()
			if leaked != nil {
				t.Fatalf("call %d: the health hook's panic reached the preflight's caller: %v", call, leaked)
			}
			synctest.Wait()
			wantNoFlight(t, cr.cityPath, "after a recover that panicked")
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("health calls = %d, want 2: the second call starts a fresh recover", got)
		}
		wantLogCount(t, &stderr, "dolt exploded", 2, "two contained panics")
	})
}

// Nothing to recover means no flight, no wait and no health call: a published
// port, a Dolt the lifecycle does not own, or an ownership check that failed
// all return at once exactly as before.
func TestManagedDoltPreflightStartsNoFlightWhenThereIsNothingToRecover(t *testing.T) {
	tests := []struct {
		name      string
		owned     func(string) (bool, error)
		port      func(string) string
		wantInLog string
	}{
		{
			name:  "port published",
			owned: func(string) (bool, error) { return true, nil },
			port:  func(string) string { return "3307" },
		},
		{
			name:  "dolt not owned by the lifecycle",
			owned: func(string) (bool, error) { return false, nil },
			port:  func(string) string { return "" },
		},
		{
			name:      "ownership unreadable",
			owned:     func(string) (bool, error) { return false, errors.New("canonical endpoint unreadable") },
			port:      func(string) string { return "" },
			wantInLog: "gc test: managed dolt ownership preflight: canonical endpoint unreadable",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr lockedBuffer
			var calls atomic.Int32
			cr := managedDoltPreflightRuntime(t, func(context.Context, string) error {
				calls.Add(1)
				return nil
			}, &stderr)
			cr.managedDoltOwned = tc.owned
			cr.managedDoltPort = tc.port

			cr.ensureManagedDoltPublishedForTick(context.Background())

			if got := calls.Load(); got != 0 {
				t.Fatalf("health calls = %d, want 0", got)
			}
			wantNoFlight(t, cr.cityPath, tc.name)
			if tc.wantInLog == "" && stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantInLog) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.wantInLog)
			}
		})
	}
}
