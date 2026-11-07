package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wantOrphanScanBudget is how long a start waits for its pre-start orphan scan
// when its context allows longer. These tests run on synctest's fake clock.
const wantOrphanScanBudget = 10 * time.Second

// orphanScanWatchdog is far past wantOrphanScanBudget. It exists only so that an
// unbounded wait fails the test with a message instead of deadlocking the
// synctest bubble.
const orphanScanWatchdog = 5 * time.Minute

// gatedScanProvider is a Fake whose process-table scans are scripted per call.
// A scan for which wedged reports true (scans count from 1) behaves like a
// wedged /proc read or a stalled tmux server: it ignores everything until the
// test opens the gate, then answers with the roots answer returns for it.
// Everything a scan goroutine touches is atomic, guarded or set before the
// first scan, because an abandoned scan returns after its caller has moved on.
type gatedScanProvider struct {
	*runtime.Fake
	city   string
	wedged func(n int64) bool
	// answer is the roots scan n (counting from 1) returns, stamped with the
	// scanned session and the city; nil answers no roots.
	answer   func(n int64) []runtime.LiveRuntime
	scanErr  error
	startErr error // Start fails with it after counting, if set

	release chan struct{}
	once    sync.Once
	entered chan struct{} // receives once per scan that blocks on the gate

	scans  atomic.Int64
	starts atomic.Int64

	mu         sync.Mutex
	terminated []int
	terminate  func(live runtime.LiveRuntime) error
}

func newGatedScanProvider(city string, wedged func(n int64) bool) *gatedScanProvider {
	return &gatedScanProvider{
		Fake:    runtime.NewFake(),
		city:    city,
		wedged:  wedged,
		release: make(chan struct{}),
		entered: make(chan struct{}, 16),
	}
}

// Scan scripts for newGatedScanProvider.
func neverWedged(int64) bool  { return false }
func alwaysWedged(int64) bool { return true }
func (p *gatedScanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	n := p.scans.Add(1)
	if p.wedged(n) {
		p.entered <- struct{}{}
		<-p.release
	}
	if p.answer == nil {
		return nil, p.scanErr
	}
	out := p.answer(n)
	for i := range out {
		if out[i].SessionID == "" {
			out[i].SessionID = id
		}
		if out[i].City == "" {
			out[i].City = p.city
		}
	}
	return out, p.scanErr
}

func (p *gatedScanProvider) TerminateRuntime(live runtime.LiveRuntime) error {
	p.mu.Lock()
	p.terminated = append(p.terminated, live.PID)
	hook := p.terminate
	p.mu.Unlock()
	if hook != nil {
		return hook(live)
	}
	return nil
}

func (p *gatedScanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.starts.Add(1)
	if p.startErr != nil {
		return p.startErr
	}
	return p.Fake.Start(ctx, name, cfg)
}

// unblock opens the gate. It is idempotent so tests can both call it and defer
// it: the deferred call frees a wedged scan when a failed assertion ends the
// test early, which would otherwise deadlock the bubble.
func (p *gatedScanProvider) unblock() {
	p.once.Do(func() { close(p.release) })
}

func (p *gatedScanProvider) terminatedPIDs() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.terminated...)
}

// orphanAt is one untracked same-session root, the shape the pre-start kill
// terminates.
func orphanAt(pid int) []runtime.LiveRuntime {
	return []runtime.LiveRuntime{{PID: pid}}
}

// killWithWatchdog runs killExistingOrphans on its own goroutine and fails the
// test if it has not returned within orphanScanWatchdog of fake time.
func killWithWatchdog(ctx context.Context, t *testing.T, mgr *Manager, sessionID string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- mgr.killExistingOrphans(ctx, sessionID) }()
	select {
	case err := <-done:
		return err
	case <-time.After(orphanScanWatchdog):
		t.Fatalf("killExistingOrphans(%s) still blocked %v after it began: the pre-start orphan scan is unbounded", sessionID, orphanScanWatchdog)
		return nil
	}
}

func wantUnansweredScan(t *testing.T, err error, cause error) {
	t.Helper()
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable so the start defers", err)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap %v", err, cause)
	}
}

// AC-1: a scan that never answers ends the start's wait at the 10-second
// budget, unavailable, and nothing it would have reported is terminated, not
// even once its late answer lands.
func TestKillExistingOrphansDefersWhenScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		sp.answer = func(int64) []runtime.LiveRuntime { return orphanAt(100) }
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		start := time.Now()

		err := killWithWatchdog(context.Background(), t, mgr, "sid-wedged")

		wantUnansweredScan(t, err, context.DeadlineExceeded)
		if elapsed := time.Since(start); elapsed != wantOrphanScanBudget {
			t.Fatalf("returned after %v, want exactly the %v budget", elapsed, wantOrphanScanBudget)
		}
		sp.unblock()
		synctest.Wait()
		if got := sp.terminatedPIDs(); len(got) != 0 {
			t.Fatalf("terminated %v from an unfinished scan, want nothing", got)
		}
	})
}

// AC-1: a start whose own deadline comes before the budget ends the wait at
// that deadline.
func TestKillExistingOrphansBudgetIsCappedByContextDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		const startDeadline = 2 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), startDeadline)
		defer cancel()
		start := time.Now()

		err := killWithWatchdog(ctx, t, mgr, "sid-wedged")

		wantUnansweredScan(t, err, context.DeadlineExceeded)
		if elapsed := time.Since(start); elapsed != startDeadline {
			t.Fatalf("returned after %v, want the start's own %v deadline", elapsed, startDeadline)
		}
	})
}

// AC-1: a start canceled while its scan is out stops waiting at once.
func TestKillExistingOrphansDefersWhenStartCanceledMidScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		sp.answer = func(int64) []runtime.LiveRuntime { return orphanAt(100) }
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-sp.entered
			time.Sleep(time.Second)
			cancel()
		}()
		start := time.Now()

		err := killWithWatchdog(ctx, t, mgr, "sid-wedged")

		wantUnansweredScan(t, err, context.Canceled)
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("returned after %v, want the moment of cancellation (1s)", elapsed)
		}
		sp.unblock()
		synctest.Wait()
		if got := sp.terminatedPIDs(); len(got) != 0 {
			t.Fatalf("terminated %v from an unfinished scan, want nothing", got)
		}
	})
}

// AC-1: a start whose context has already ended launches no scan.
func TestKillExistingOrphansWithEndedContextStartsNoScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, neverWedged)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := killWithWatchdog(ctx, t, mgr, "sid-ended")

		wantUnansweredScan(t, err, context.Canceled)
		if got := sp.scans.Load(); got != 0 {
			t.Fatalf("%d scans launched under an ended context, want 0", got)
		}
	})
}

// AC-1: a scan that answers inside the budget is used: its orphan is
// terminated and the start proceeds.
func TestKillExistingOrphansUsesAnAnswerWithinTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		sp := newGatedScanProvider(city, alwaysWedged)
		sp.answer = func(int64) []runtime.LiveRuntime { return orphanAt(100) }
		defer sp.unblock()
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))
		go func() {
			<-sp.entered
			time.Sleep(wantOrphanScanBudget - time.Second)
			sp.unblock()
		}()

		err := killWithWatchdog(context.Background(), t, mgr, "sid-slow")
		if err != nil {
			t.Fatalf("killExistingOrphans = %v, want nil for a scan answered within the budget", err)
		}
		if got := sp.terminatedPIDs(); len(got) != 1 || got[0] != 100 {
			t.Fatalf("terminated %v, want the answered orphan 100", got)
		}
	})
}
