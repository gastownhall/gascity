//go:build !windows

package runtime

import (
	"errors"
	"testing"
	"time"
)

// reapTestHang is the longest a test waits for a terminate call before it
// declares the call hung. It is a hang guard, never a timing assertion.
const reapTestHang = 5 * time.Second

// terminateResult runs the two-grace terminate on a nil command (no process to
// signal) and reports its elapsed time and error, failing the test if the call never returns.
func terminateResult(t *testing.T, done <-chan struct{}, grace, reapGrace time.Duration) (time.Duration, error) {
	t.Helper()
	type outcome struct {
		err     error
		elapsed time.Duration
	}
	res := make(chan outcome, 1)
	go func() {
		start := time.Now()
		err := terminateManagedProcessGraces(nil, done, grace, reapGrace)
		res <- outcome{err, time.Since(start)}
	}()
	select {
	case o := <-res:
		return o.elapsed, o.err
	case <-time.After(reapTestHang):
		t.Fatalf("terminate still blocked after %v: it waits forever once SIGKILL has been sent", reapTestHang)
		return 0, nil
	}
}

// Kills: the unbounded `<-done` after SIGKILL. A process that outlives its own
// SIGKILL (uninterruptible sleep) must surface as not-confirmed-dead within the
// reap grace instead of blocking the caller forever.
func TestTerminateManagedProcessNotConfirmedDeadWhenDoneNeverCloses(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	const grace, reapGrace = 10 * time.Millisecond, 30 * time.Millisecond
	elapsed, err := terminateResult(t, done, grace, reapGrace)
	if !errors.Is(err, ErrProcessNotConfirmedDead) {
		t.Fatalf("terminate error = %v, want ErrProcessNotConfirmedDead", err)
	}
	if elapsed < reapGrace {
		t.Errorf("terminate returned after %v, before the %v reap grace ran out", elapsed, reapGrace)
	}
}

// Kills: a reap bound that reports a process dead-late as not confirmed dead.
func TestTerminateManagedProcessDoneInsideReapGraceIsSuccess(t *testing.T) {
	done := make(chan struct{})
	const grace, reapGrace = 10 * time.Millisecond, 3 * time.Second
	timer := time.AfterFunc(60*time.Millisecond, func() { close(done) })
	t.Cleanup(func() {
		if timer.Stop() {
			close(done)
		}
	})

	_, err := terminateResult(t, done, grace, reapGrace)
	if err != nil {
		t.Fatalf("terminate error = %v, want nil: done closed inside the reap grace", err)
	}
}

// Kills: a sentinel whose text trips IsSessionGone's substring checks, and one
// that is also a refused or unsupported stop (callers dispatch on those).
func TestProcessNotConfirmedDeadIsNeitherGoneNorRefused(t *testing.T) {
	for _, gone := range []string{"session not found", "not running", "not found", "no tmux server running"} {
		if containsFold(ErrProcessNotConfirmedDead.Error(), gone) {
			t.Errorf("sentinel text %q contains the session-gone marker %q", ErrProcessNotConfirmedDead, gone)
		}
	}
	if errors.Is(ErrProcessNotConfirmedDead, ErrStopRefused) || errors.Is(ErrProcessNotConfirmedDead, ErrStopUnsupported) {
		t.Error("ErrProcessNotConfirmedDead must not wrap ErrStopRefused or ErrStopUnsupported")
	}
}

// Kills: the bounds drifting apart: a client that waits only as long as the
// owner's stop grace gives up before the owner has finished the reap wait.
func TestManagedProcessReapGraceIsBounded(t *testing.T) {
	if ManagedProcessReapGrace <= 0 || ManagedProcessReapGrace >= ManagedProcessStopGrace {
		t.Fatalf("ManagedProcessReapGrace = %v, want a positive bound below the %v stop grace", ManagedProcessReapGrace, ManagedProcessStopGrace)
	}
}
