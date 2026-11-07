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

// These tests run on synctest's fake clock, so waiting out the production
// budget (orphanScanBudget, the 10 s bound killExistingOrphans gives its
// scan) costs no wall time. orphanScanWatchdog is far past that budget: it
// exists only so a wedged scan fails the test with a message instead of
// deadlocking the synctest bubble.
const orphanScanWatchdog = 5 * time.Minute

// scanStep scripts one FindRuntimesBySessionID call: delay waits (virtual
// time) before answering, block wedges the scan until the channel closes, and
// then the call answers roots and err.
type scanStep struct {
	roots []runtime.LiveRuntime
	err   error
	block chan struct{}
	delay time.Duration
}

// scriptedScanProvider is a Fake whose process-table scan is scripted per
// call, so a test decides when the pre-start scan answers, fails, or wedges
// like a stalled tmux server. Steps are consumed in order; with none queued
// the scan answers an immediate empty result. Terminations are recorded, and
// onTerminate runs before the nth one (1-based), so a test can cancel the
// caller between terminations. startErr, when set, is every Start's answer,
// as a provider whose launches die during startup.
type scriptedScanProvider struct {
	*runtime.Fake
	city        string
	mu          sync.Mutex
	steps       []scanStep
	started     chan struct{} // one buffered send per scan begun
	scans       atomic.Int64
	starts      atomic.Int64
	terminated  []runtime.LiveRuntime
	onTerminate func(n int)
	startErr    error
	killErrPID  int // a TerminateRuntime of this PID fails; 0 fails none
}

func newScriptedScanProvider(city string, steps ...scanStep) *scriptedScanProvider {
	return &scriptedScanProvider{
		Fake:    runtime.NewFake(),
		city:    city,
		steps:   steps,
		started: make(chan struct{}, 64),
	}
}

func (p *scriptedScanProvider) queue(step scanStep) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steps = append(p.steps, step)
}

func (p *scriptedScanProvider) FindRuntimesBySessionID(id string) ([]runtime.LiveRuntime, error) {
	p.mu.Lock()
	step := scanStep{}
	if len(p.steps) > 0 {
		step, p.steps = p.steps[0], p.steps[1:]
	}
	p.mu.Unlock()
	p.scans.Add(1)
	p.started <- struct{}{}
	if step.delay > 0 {
		<-time.After(step.delay)
	}
	if step.block != nil {
		<-step.block
	}
	out := make([]runtime.LiveRuntime, len(step.roots))
	copy(out, step.roots)
	for i := range out {
		if out[i].SessionID == "" {
			out[i].SessionID = id
		}
		if out[i].City == "" {
			out[i].City = p.city
		}
	}
	return out, step.err
}

func (p *scriptedScanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.starts.Add(1)
	if p.startErr != nil {
		return p.startErr
	}
	return p.Fake.Start(ctx, name, cfg)
}

func (p *scriptedScanProvider) TerminateRuntime(r runtime.LiveRuntime) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminated = append(p.terminated, r)
	n := len(p.terminated)
	if p.onTerminate != nil {
		p.onTerminate(n)
	}
	if p.killErrPID != 0 && r.PID == p.killErrPID {
		return fmt.Errorf("orphan pid=%d survived SIGKILL", r.PID)
	}
	return nil
}

func (p *scriptedScanProvider) terminations() []runtime.LiveRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]runtime.LiveRuntime(nil), p.terminated...)
}

func (p *scriptedScanProvider) terminatedPIDs() []int {
	out := []int{}
	for _, live := range p.terminations() {
		out = append(out, live.PID)
	}
	return out
}

// withOrphanScanWatchdog runs f on its own goroutine and fails the test if it
// has not returned within orphanScanWatchdog of fake time, so a wedged
// pre-start scan cannot deadlock the synctest bubble.
func withOrphanScanWatchdog(t *testing.T, what string, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(orphanScanWatchdog):
		t.Fatalf("%s still blocked %v after the scan budget: the pre-start scan is unbounded", what, orphanScanWatchdog)
		return nil
	}
}

// TestKillExistingOrphansDefersWhenScanNeverFinishes pins the bound itself: a
// pre-start scan that never answers ends at its budget with an error wrapping
// runtime.ErrRuntimeUnavailable (the start defers), and no root is terminated
// from a scan that answered nothing.
func TestKillExistingOrphansDefersWhenScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		sp := newScriptedScanProvider(t.TempDir(), scanStep{block: release})
		defer close(release)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		begin := time.Now()

		err := withOrphanScanWatchdog(t, "killExistingOrphans", func() error {
			return mgr.killExistingOrphans(context.Background(), "gc-wedged")
		})

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable so the start defers", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap the expired scan budget (context.DeadlineExceeded)", err)
		}
		if elapsed := time.Since(begin); elapsed < orphanScanBudget || elapsed > orphanScanBudget+5*time.Second {
			t.Fatalf("returned after %v, want at the %v budget", elapsed, orphanScanBudget)
		}
		if got := sp.terminations(); len(got) != 0 {
			t.Fatalf("terminated %v from a scan that answered nothing", got)
		}
	})
}

// TestKillExistingOrphansDefersUnfinishedScanOnCallerDeadline: a caller whose
// context ends before the budget gets its shorter deadline, not the full wait.
func TestKillExistingOrphansDefersUnfinishedScanOnCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		sp := newScriptedScanProvider(t.TempDir(), scanStep{block: release})
		defer close(release)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		begin := time.Now()

		err := withOrphanScanWatchdog(t, "killExistingOrphans", func() error {
			return mgr.killExistingOrphans(ctx, "gc-wedged")
		})

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable and DeadlineExceeded", err)
		}
		if elapsed := time.Since(begin); elapsed >= orphanScanBudget {
			t.Fatalf("returned after %v, want the caller's 3s deadline, not the %v budget", elapsed, orphanScanBudget)
		}
		if got := sp.terminations(); len(got) != 0 {
			t.Fatalf("terminated %v from a scan that answered nothing", got)
		}
	})
}

// TestKillExistingOrphansDefersUnfinishedScanOnCallerCancel: a caller that
// cancels mid-scan gets its cancellation, and nothing is terminated.
func TestKillExistingOrphansDefersUnfinishedScanOnCallerCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		sp := newScriptedScanProvider(t.TempDir(), scanStep{block: release})
		defer close(release)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- mgr.killExistingOrphans(ctx, "gc-wedged") }()
		<-sp.started // the scan is under way
		cancel()

		select {
		case err := <-done:
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable and context.Canceled", err)
			}
			if got := sp.terminations(); len(got) != 0 {
				t.Fatalf("terminated %v from a scan that answered nothing", got)
			}
		case <-time.After(orphanScanWatchdog):
			t.Fatalf("killExistingOrphans still blocked after the caller canceled: the scan is unbounded")
		}
	})
}

// TestKillExistingOrphansAlreadyEndedContextLaunchesNoScan: an ended context
// starts nothing, so a refused start cannot even leak a scan.
func TestKillExistingOrphansAlreadyEndedContextLaunchesNoScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedScanProvider(t.TempDir())
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := mgr.killExistingOrphans(ctx, "gc-wedged")

		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want it to wrap runtime.ErrRuntimeUnavailable and context.Canceled", err)
		}
		if n := sp.scans.Load(); n != 0 {
			t.Fatalf("scans begun = %d, want 0: an ended context launches no scan", n)
		}
	})
}

// TestKillExistingOrphansUsesTheAnswerThatBeatsTheBudget: a scan that answers
// within the budget is a completed scan, and its roots are terminated.
func TestKillExistingOrphansUsesTheAnswerThatBeatsTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedScanProvider(t.TempDir(), scanStep{
			roots: []runtime.LiveRuntime{{PID: 4100, IsTracked: false}},
			delay: orphanScanBudget - 2*time.Second,
		})
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))

		err := withOrphanScanWatchdog(t, "killExistingOrphans", func() error {
			return mgr.killExistingOrphans(context.Background(), "gc-slow")
		})
		if err != nil {
			t.Fatalf("killExistingOrphans = %v, want nil: the answer beat the budget", err)
		}
		if got := sp.terminatedPIDs(); len(got) != 1 || got[0] != 4100 {
			t.Fatalf("terminated %v, want the answered root 4100", got)
		}
	})
}

// TestKillExistingOrphansScansOncePerCityAndSession: while one scan of a
// city/session key is outstanding, a retry — through the same Manager, a
// replacement Manager, or an equivalent spelling of the city path — starts no
// second scan and is refused at once, while a different session or city scans
// independently.
func TestKillExistingOrphansScansOncePerCityAndSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		city := t.TempDir()
		release := make(chan struct{})
		sp := newScriptedScanProvider(city, scanStep{block: release})
		defer close(release)
		store := beads.NewMemStore()
		mgr1 := newTestManager(t, store, sp, WithCityPath(city))
		first := make(chan error, 1)
		go func() { first <- mgr1.killExistingOrphans(context.Background(), "gc-a") }()
		<-sp.started

		for _, tc := range []struct {
			name string
			mgr  *Manager
			sid  string
		}{
			{name: "replacement Manager", mgr: newTestManager(t, store, sp, WithCityPath(city+"/")), sid: "gc-a"},
			{name: "same Manager", mgr: mgr1, sid: "gc-a"},
		} {
			begin := time.Now()
			err := withOrphanScanWatchdog(t, "killExistingOrphans retry", func() error {
				return tc.mgr.killExistingOrphans(context.Background(), tc.sid)
			})
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s: err = %v, want it to wrap runtime.ErrRuntimeUnavailable", tc.name, err)
			}
			if elapsed := time.Since(begin); elapsed >= time.Second {
				t.Fatalf("%s: refusal waited %v, want an immediate refusal", tc.name, elapsed)
			}
			if n := sp.scans.Load(); n != 1 {
				t.Fatalf("%s: scans begun = %d, want 1: no second scan for an outstanding key", tc.name, n)
			}
		}

		// A different session in the same city, and the same session in a
		// different city, are different keys: both scan independently.
		for _, tc := range []struct {
			name string
			mgr  *Manager
			sid  string
		}{
			{name: "other session, same city", mgr: mgr1, sid: "gc-b"},
			{name: "same session, other city", mgr: newTestManager(t, store, sp, WithCityPath(t.TempDir())), sid: "gc-a"},
		} {
			sp.queue(scanStep{})
			err := withOrphanScanWatchdog(t, "killExistingOrphans independent key", func() error {
				return tc.mgr.killExistingOrphans(context.Background(), tc.sid)
			})
			if err != nil {
				t.Fatalf("%s: err = %v, want an independent scan to proceed", tc.name, err)
			}
		}
		if n := sp.scans.Load(); n != 3 {
			t.Fatalf("scans begun = %d, want 3: independent keys scan independently", n)
		}
		<-first // the wedged scan's own caller still ends at its budget
	})
}

// TestKillExistingOrphansFreesTheKeyWhenTheScanReturns: a completed scan's
// key is free, so the next cleanup of that key scans again.
func TestKillExistingOrphansFreesTheKeyWhenTheScanReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := newScriptedScanProvider(t.TempDir(), scanStep{
			roots: []runtime.LiveRuntime{{PID: 4100, IsTracked: false}},
		})
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(sp.city))

		if err := mgr.killExistingOrphans(context.Background(), "gc-a"); err != nil {
			t.Fatalf("first cleanup: %v", err)
		}
		sp.queue(scanStep{roots: []runtime.LiveRuntime{{PID: 4200, IsTracked: false}}})
		if err := mgr.killExistingOrphans(context.Background(), "gc-a"); err != nil {
			t.Fatalf("second cleanup: %v", err)
		}

		if n := sp.scans.Load(); n != 2 {
			t.Fatalf("scans begun = %d, want 2: the key frees when the scan returns", n)
		}
		if got := sp.terminatedPIDs(); len(got) != 2 || got[0] != 4100 || got[1] != 4200 {
			t.Fatalf("terminated %v, want each scan's own root", got)
		}
	})
}
