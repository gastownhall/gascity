//go:build linux || darwin

package proctable

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// KillByPID terminates pid with SIGTERM, then SIGKILL after
// runtime.ManagedProcessStopGrace, then waits (bounded by
// runtime.ManagedProcessReapGrace) for the process to be confirmed dead — gone
// or a zombie — before returning. Already-gone processes are success. A process
// that survives its own SIGKILL past the reap grace (e.g. wedged in D-state
// under I/O) yields an error so callers can refuse to start a name-reused
// replacement that would race it for the same work.
func KillByPID(pid int) error {
	// Capture the target's start-time identity BEFORE signaling. During the
	// post-SIGKILL reap wait the PID can be reaped and recycled to an unrelated
	// process; without this, a recycled PID reads as "still alive" and we would
	// wrongly report a target that is actually gone as not-confirmed-dead,
	// spuriously refusing a legitimate Start. StartTime is empty on hosts
	// without /proc (darwin) or when the record is unreadable, in which case
	// runLive falls back to plain liveness — current behavior preserved.
	startTime, _ := pidutil.StartTime(pid)
	return killByPID(
		pid,
		syscall.Kill,
		pidAlive,
		func(p int) bool { return pidutil.AliveWithStartTime(p, startTime) },
		runtime.ManagedProcessStopGrace,
		runtime.ManagedProcessReapGrace,
	)
}

// killByPID is the signal/confirm core with its syscalls injected so the
// confirmed-dead-before-return contract can be unit-tested without real
// processes. termLive is the cheap kill(0) liveness used during the SIGTERM
// grace window (a zombie still counts as live here, matching prior behavior).
// runLive reports whether the process is still runnable — false once it is gone
// or a zombie, since a zombie can no longer execute and therefore cannot race a
// replacement.
func killByPID(
	pid int,
	kill func(int, syscall.Signal) error,
	termLive func(int) bool,
	runLive func(int) bool,
	grace, reapGrace time.Duration,
) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	if !termLive(pid) {
		return nil
	}
	if err := signalPIDWith(pid, syscall.SIGTERM, kill); err != nil {
		return fmt.Errorf("signal PID %d with SIGTERM: %w", pid, err)
	}
	if waitUntil(func() bool { return !termLive(pid) }, grace) {
		return nil
	}
	if err := signalPIDWith(pid, syscall.SIGKILL, kill); err != nil {
		return fmt.Errorf("signal PID %d with SIGKILL: %w", pid, err)
	}
	if waitUntil(func() bool { return !runLive(pid) }, reapGrace) {
		return nil
	}
	return fmt.Errorf("proctable: PID %d still runnable %s after SIGKILL (not confirmed dead)", pid, reapGrace)
}

// waitUntil polls done at 25ms until it reports true or timeout elapses,
// returning done's final result. Checked once up front so a zero timeout still
// observes an already-satisfied condition.
func waitUntil(done func() bool, timeout time.Duration) bool {
	if done() {
		return true
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			return done()
		case <-ticker.C:
			if done() {
				return true
			}
		}
	}
}

func signalPIDWith(pid int, sig syscall.Signal, kill func(int, syscall.Signal) error) error {
	if err := kill(-pid, sig); err == nil {
		return nil
	}
	err := kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
