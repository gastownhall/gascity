package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// wantOrphanScanBudget is how long a pre-start orphan scan is waited for when
// the caller's context allows longer.
const wantOrphanScanBudget = 10 * time.Second

// blockingScanFake is a Fake whose process-table scan blocks until release is
// closed and then answers found. It counts the scans started and records the
// roots it is asked to terminate.
type blockingScanFake struct {
	*runtime.Fake
	release     chan struct{}
	releaseOnce sync.Once
	found       []runtime.LiveRuntime

	mu         sync.Mutex
	scans      int
	terminated []runtime.LiveRuntime
}

// newBlockingScanFake returns a blocked scanner that answers found, released
// when the test ends at the latest so no scan outlives its synctest bubble.
func newBlockingScanFake(t *testing.T, found ...runtime.LiveRuntime) *blockingScanFake {
	sp := &blockingScanFake{Fake: runtime.NewFake(), release: make(chan struct{}), found: found}
	t.Cleanup(sp.answer)
	return sp
}

// answer lets every blocked and future scan return.
func (f *blockingScanFake) answer() {
	f.releaseOnce.Do(func() { close(f.release) })
}

func (f *blockingScanFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	f.mu.Lock()
	f.scans++
	f.mu.Unlock()
	<-f.release
	return f.found, nil
}

func (f *blockingScanFake) TerminateRuntime(live runtime.LiveRuntime) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, live)
	return nil
}

func (f *blockingScanFake) scanCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scans
}

func (f *blockingScanFake) terminatedRoots() []runtime.LiveRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.LiveRuntime(nil), f.terminated...)
}

// killOrphansAsync runs the pre-start orphan cleanup on its own goroutine and
// returns the channel its result lands on.
func killOrphansAsync(ctx context.Context, mgr *Manager, sessionID string) <-chan error {
	done := make(chan error, 1)
	go func() { done <- mgr.killExistingOrphans(ctx, sessionID) }()
	return done
}

// cleanupResult reports whether the cleanup has returned yet, and its result.
func cleanupResult(done <-chan error) (bool, error) {
	select {
	case err := <-done:
		return true, err
	default:
		return false, nil
	}
}

// requireUnanswered fails unless the cleanup has returned an error that says
// the scan gave no answer for want's reason.
func requireUnanswered(t *testing.T, done <-chan error, want error) {
	t.Helper()
	ok, err := cleanupResult(done)
	if !ok {
		t.Fatal("pre-start orphan cleanup is still waiting on the blocked scan")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("cleanup error = %v, want one wrapping runtime.ErrRuntimeUnavailable", err)
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("cleanup error = %v, want one wrapping %v", err, want)
	}
}

// releaseScan lets a blocked scan answer, waits for everything it started to
// settle, and fails if anything was terminated from its abandoned answer.
func releaseScan(t *testing.T, sp *blockingScanFake) {
	t.Helper()
	sp.answer()
	synctest.Wait()
	if got := sp.terminatedRoots(); len(got) != 0 {
		t.Fatalf("terminated %v from a scan the cleanup had stopped waiting for", got)
	}
}

// strandedOrphan is an untracked root of sessionID in city, which a completed
// scan would have the cleanup terminate.
func strandedOrphan(city, sessionID string) runtime.LiveRuntime {
	return runtime.LiveRuntime{SessionID: sessionID, City: city, PID: 100}
}

func TestKillExistingOrphansStopsWaitingAtTheScanBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-budget"
		city := t.TempDir()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

		done := killOrphansAsync(context.Background(), mgr, sessionID)
		time.Sleep(wantOrphanScanBudget - time.Nanosecond)
		synctest.Wait()
		if ok, err := cleanupResult(done); ok {
			t.Errorf("cleanup returned %v before the scan budget ran out", err)
		} else {
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			requireUnanswered(t, done, context.DeadlineExceeded)
		}
		releaseScan(t, sp)
	})
}

func TestKillExistingOrphansReturnsWhenScanNeverFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-deadline"
		city := t.TempDir()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		done := killOrphansAsync(ctx, mgr, sessionID)
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		requireUnanswered(t, done, context.DeadlineExceeded)
		releaseScan(t, sp)
	})
}

func TestKillExistingOrphansStopsWaitingWhenCanceledMidScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-cancel"
		city := t.TempDir()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

		ctx, cancel := context.WithCancel(context.Background())
		done := killOrphansAsync(ctx, mgr, sessionID)
		synctest.Wait()
		if got := sp.scanCount(); got != 1 {
			t.Errorf("scans started = %d, want 1 in flight before the cancel", got)
		}
		cancel()
		synctest.Wait()
		requireUnanswered(t, done, context.Canceled)
		releaseScan(t, sp)
	})
}

func TestKillExistingOrphansStartsNoScanForAnEndedContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-ended"
		city := t.TempDir()
		sp := newBlockingScanFake(t, strandedOrphan(city, sessionID))
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := killOrphansAsync(ctx, mgr, sessionID)
		synctest.Wait()
		requireUnanswered(t, done, context.Canceled)
		if got := sp.scanCount(); got != 0 {
			t.Errorf("scans started = %d, want none under an ended context", got)
		}
		releaseScan(t, sp)
	})
}

func TestKillExistingOrphansUsesAScanThatAnswersInTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sessionID = "sid-in-time"
		city := t.TempDir()
		orphan := strandedOrphan(city, sessionID)
		sp := newBlockingScanFake(t, orphan)
		mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(city))

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done := killOrphansAsync(ctx, mgr, sessionID)
		time.Sleep(time.Second)
		sp.answer()
		synctest.Wait()
		ok, err := cleanupResult(done)
		if !ok {
			t.Fatal("cleanup still waiting after the scan answered")
		}
		if err != nil {
			t.Fatalf("cleanup error = %v, want nil for an orphan terminated in time", err)
		}
		if got := sp.terminatedRoots(); len(got) != 1 || got[0].PID != orphan.PID {
			t.Fatalf("terminated %v, want only the orphan pid %d the scan answered", got, orphan.PID)
		}
	})
}
