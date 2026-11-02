package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// hangingScanFake is a Fake whose process-table scan never returns until the
// test releases it, as a wedged process-table read would.
type hangingScanFake struct {
	*runtime.Fake
	release chan struct{}
}

func (f *hangingScanFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	<-f.release
	return nil, nil
}

// A pre-start orphan scan that never finishes must not wedge the start: once
// the start context's deadline passes, killExistingOrphans must return an
// error (the start defers) rather than block forever.
func TestKillExistingOrphansReturnsWhenScanNeverFinishes(t *testing.T) {
	sp := &hangingScanFake{Fake: runtime.NewFake(), release: make(chan struct{})}
	t.Cleanup(func() { close(sp.release) })
	mgr := newTestManager(t, beads.NewMemStore(), sp)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mgr.killExistingOrphans(ctx, "sid-wedged") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("killExistingOrphans = nil for an unfinished scan, want an error so the start defers")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killExistingOrphans still blocked 5s after the start context's 50ms deadline: the wedged scan is unbounded")
	}
}

// orphanScanBudgetValue pins the pre-start scan's wait bound. Tests run on
// synctest's fake clock, so waiting it out costs no wall time.
const orphanScanBudgetValue = 10 * time.Second

// orphanWatchdogLimit is far past the scan budget. It exists only so that an
// unbounded wait fails the test with a message instead of deadlocking.
const orphanWatchdogLimit = 5 * time.Minute

// orphanWatchdog runs fn on its own goroutine and fails the test if it has
// not returned within orphanWatchdogLimit, so an unbounded pre-start orphan
// scan fails the test instead of hanging it.
func orphanWatchdog(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(orphanWatchdogLimit):
		t.Fatalf("the gated start still blocked %v after its scan budget ended: the pre-start orphan scan is unbounded", orphanWatchdogLimit)
		return nil
	}
}

// scriptedOrphanScan is a scripted pre-start process-table scanner over a
// Fake. Calls numbered up to blockLeading, and from blockedFrom on when it is
// set, wait on release (a wedged scan); the rest answer at once with whatever
// answer holds then. onBlocked fires when a blocked scan has been entered,
// onTerminate before each termination, and the first startFailures Starts fail
// as a died-during-startup resume would.
type scriptedOrphanScan struct {
	*runtime.Fake
	release       chan struct{}
	once          sync.Once
	blockLeading  atomic.Int64
	blockedFrom   atomic.Int64
	startFailures atomic.Int64
	calls         atomic.Int64
	running       atomic.Int64
	starts        atomic.Int64
	mu            sync.Mutex
	roots         []runtime.LiveRuntime
	err           error
	onBlocked     func()
	onTerminate   func(pid int)
	terminateErrs map[int]error
}

func newScriptedOrphanScan() *scriptedOrphanScan {
	return &scriptedOrphanScan{Fake: runtime.NewFake(), release: make(chan struct{})}
}

func (f *scriptedOrphanScan) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	n := f.calls.Add(1)
	f.running.Add(1)
	defer f.running.Add(-1)
	if n <= f.blockLeading.Load() || (f.blockedFrom.Load() > 0 && n >= f.blockedFrom.Load()) {
		if hook := f.onBlocked; hook != nil {
			hook()
		}
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.roots, f.err
}

// setAnswer scripts what answering scans return.
func (f *scriptedOrphanScan) setAnswer(roots []runtime.LiveRuntime, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.roots, f.err = roots, err
}

// unblock lets every blocked scan return. It is idempotent so tests can both
// call it and defer it: the deferred call frees a wedged goroutine when a
// failed assertion ends the test early.
func (f *scriptedOrphanScan) unblock() { f.once.Do(func() { close(f.release) }) }

func (f *scriptedOrphanScan) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if n := f.starts.Add(1); n <= f.startFailures.Load() {
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
	}
	return f.Fake.Start(ctx, name, cfg)
}

func (f *scriptedOrphanScan) TerminateRuntime(live runtime.LiveRuntime) error {
	f.mu.Lock()
	hook, fail := f.onTerminate, f.terminateErrs[live.PID]
	f.mu.Unlock()
	if hook != nil {
		hook(live.PID)
	}
	if fail != nil {
		return fail
	}
	return f.Fake.TerminateRuntime(live)
}

// orphanScanTerminatedPIDs lists the PIDs the scanner terminated, in order.
func orphanScanTerminatedPIDs(sp *scriptedOrphanScan) []string {
	var pids []string
	for _, c := range sp.Calls {
		if c.Method == "TerminateRuntime" {
			pids = append(pids, c.Value)
		}
	}
	return pids
}

// A scan that never answers must defer the start at the scan budget with an
// unavailable error, not wait for the scan forever: the scan being wedged
// says nothing about whether orphans exist.
func TestKillExistingOrphansDefersWhenScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		start := time.Now()

		err := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "sid-wedged") })

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("killExistingOrphans = %v, want unavailable wrapping context.DeadlineExceeded at the scan budget", err)
		}
		if elapsed := time.Since(start); elapsed > orphanScanBudgetValue {
			t.Fatalf("returned after %v, want within the %v scan budget", elapsed, orphanScanBudgetValue)
		}
		if pids := orphanScanTerminatedPIDs(sp); len(pids) != 0 {
			t.Fatalf("terminated %v without an answer, want nothing", pids)
		}
	})
}

// The caller's own earlier deadline caps the scan wait: a start that can only
// wait 2s must not hold its slot for the whole 10s budget.
func TestKillExistingOrphansBudgetIsCappedByContextDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()

		err := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(ctx, "sid-capped") })

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("killExistingOrphans = %v, want unavailable wrapping the caller's DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second || elapsed >= orphanScanBudgetValue {
			t.Fatalf("returned after %v, want at the caller's 2s deadline, not the %v budget", elapsed, orphanScanBudgetValue)
		}
	})
}

// A start context canceled while the scan is running ends the wait the same
// way a deadline does.
func TestKillExistingOrphansMidScanCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sp.onBlocked = func() { time.AfterFunc(time.Millisecond, cancel) }

		err := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(ctx, "sid-cancel") })

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("killExistingOrphans = %v, want unavailable wrapping context.Canceled", err)
		}
		if pids := orphanScanTerminatedPIDs(sp); len(pids) != 0 {
			t.Fatalf("terminated %v without an answer, want nothing", pids)
		}
	})
}

// An already-ended start context starts no scan at all: there is no point
// sweeping for orphans of a start that has been given up.
func TestKillExistingOrphansAlreadyEndedContextStartsNoScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := mgr.killExistingOrphans(ctx, "sid-gone")

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("killExistingOrphans = %v, want unavailable wrapping context.Canceled", err)
		}
		if got := sp.calls.Load(); got != 0 {
			t.Fatalf("scans = %d under an already-canceled context, want 0", got)
		}
	})
}

// An answer that arrives before the deadline is used, and its roots are
// terminated as before: the bound adds a ceiling, not a floor.
func TestKillExistingOrphansAnswerBeforeTheDeadlineIsUsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedOrphanScan()
		sp.blockLeading.Store(1)
		mgr := newTestManager(t, beads.NewMemStore(), sp)
		city := mgr.cityPath
		sp.setAnswer([]runtime.LiveRuntime{{SessionID: "sid-late", City: city, PID: 100}}, nil)
		sp.onBlocked = func() { time.AfterFunc(time.Second, sp.unblock) }

		err := orphanWatchdog(t, func() error { return mgr.killExistingOrphans(context.Background(), "sid-late") })
		if err != nil {
			t.Fatalf("killExistingOrphans = %v, want nil for a scan that answered in time", err)
		}
		if pids := orphanScanTerminatedPIDs(sp); len(pids) != 1 || pids[0] != "100" {
			t.Fatalf("terminated %v, want the answered scan's root 100", pids)
		}
	})
}
