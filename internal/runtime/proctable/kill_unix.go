//go:build linux || darwin

package proctable

import (
	"errors"
	"fmt"
	"log"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// KillByPID's dependencies are indirected so a test can prove the
// infrastructure refusal below is wired into KillByPID itself, not merely that
// the classifier works in isolation. That distinction is the lesson of
// gastownhall/gascity#5837: the first guard for that class was correct, tested,
// and installed on a different kill family than the one that fired. The vars
// are test-only injection points, unsynchronized; the package's tests do not
// run in parallel, and stubKillByPIDDeps restores every one of them.
var (
	killByPIDInfrastructureTarget = IsCityInfrastructureRoot
	killByPIDSignal               = syscall.Kill
	killByPIDLiveness             = killLivenessFuncsForPID
)

// KillByPID terminates pid with SIGTERM, then SIGKILL after
// runtime.ManagedProcessStopGrace, then waits (bounded by
// runtime.ManagedProcessReapGrace) for the process to be confirmed dead — gone
// or a zombie — before returning. Already-gone processes are success. A process
// that survives its own SIGKILL past the reap grace (e.g. wedged in D-state
// under I/O) yields an error so callers can refuse to start a name-reused
// replacement that would race it for the same work. A target classified as
// city infrastructure by IsCityInfrastructureRoot (a tmux server or client,
// the managed Dolt scope watchdog, bd's db-proxy-child) is never signaled: the
// call logs the refusal and returns nil, since such a process cannot race an
// agent replacement and an error would block every gated start behind it.
func KillByPID(pid int) error {
	// Never signal city infrastructure. signalPIDWith signals the process
	// group first, which for a tmux server is the server itself — one socket
	// is one server is every session in the city. The scanner has excluded
	// tmux from the agent-root set since gastownhall/gascity#5837, and the
	// orphan sweep and pre-start kill fence the Dolt watchdog and bd proxy at
	// their own call sites, so no live caller hands infrastructure in today;
	// this is defense in depth at the kill family itself, which two providers
	// reach with whatever PID they hold. It classifies the target at this
	// instant, which narrows, not closes, the window for a PID recycled before
	// the signal (gastownhall/gascity#6172). Refusing is success, not an
	// error: infrastructure cannot race a replacement for an agent's work, so
	// there is no orphan to refuse a Start over, and an error here would
	// propagate through every gated start. The refusal logs because it should
	// fire approximately never.
	if killByPIDInfrastructureTarget(pid) {
		log.Printf("proctable: refusing to kill PID %d: city infrastructure, not an agent runtime; signaling its process group would take the city down with it (gastownhall/gascity#6172)", pid)
		return nil
	}
	// Capture the target's start-time identity BEFORE signaling. During the
	// post-SIGKILL reap wait the PID can be reaped and recycled to an unrelated
	// process; without this, a recycled PID reads as "still alive" and we would
	// wrongly report a target that is actually gone as not-confirmed-dead,
	// spuriously refusing a legitimate Start. StartTime reads /proc where it
	// exists and falls back to ps elsewhere, so it is empty only when neither
	// mechanism can answer, in which case runLive falls back to plain liveness
	// — current behavior preserved.
	termLive, runLive := killByPIDLiveness(pid)
	return killByPID(
		pid,
		killByPIDSignal,
		termLive,
		runLive,
		runtime.ManagedProcessStopGrace,
		runtime.ManagedProcessReapGrace,
	)
}

// killLivenessFuncsForPID builds the two liveness probes KillByPID signals
// against, both bound to the target's start-time identity captured up front.
//
// Extracted so the IDENTITY BINDING itself is testable: both probes must report
// false for a different live PID, which is what stops a recycled PID from
// receiving a process-group SIGKILL. A version that returns bare existence
// checks passes every process-level test and still ships the bug.
func killLivenessFuncsForPID(pid int) (termLive, runLive func(int) bool) {
	startTime, _ := pidutil.StartTime(pid)
	return func(p int) bool { return pidAliveWithIdentity(p, startTime) },
		func(p int) bool { return pidutil.AliveWithStartTime(p, startTime) }
}

// pidAliveWithIdentity is the cheap kill(0) liveness used across the SIGTERM
// grace, plus recycled-PID protection.
//
// The bare kill(0) form was pure existence with zero identity, and it is what
// gates the SIGKILL wave: a target that answered SIGTERM and was reaped, whose
// PID was then recycled by an unrelated process before the grace expired, still
// read as "alive" — so SIGKILL was sent to it. signalPIDWith tries kill(-pid)
// first, so if the recycled PID happens to lead a process group (every tmux pane
// command and every Setpgid'd daemon does) the entire unrelated group dies and
// the call returns success. Validating the start-time identity on every poll
// means a recycled PID reads as dead, waitUntil succeeds, and no second wave is
// ever sent.
//
// Zombie semantics are preserved deliberately: this keeps kill(0)'s view, in
// which a zombie still counts as live, matching the pre-existing SIGTERM-grace
// behavior. Only the identity check is added. An unreadable or absent start time
// keeps the conservative "still alive" answer rather than inventing a death.
func pidAliveWithIdentity(pid int, startTime string) bool {
	if !pidAlive(pid) {
		return false
	}
	if startTime == "" {
		return true
	}
	current, err := pidutil.StartTime(pid)
	if err != nil {
		return true
	}
	return current == startTime
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
	// Re-validate identity immediately before the second wave. waitUntil's last
	// poll already implies this, but the check is written out because it is a
	// contract, not an optimization: every signal wave is preceded by a fresh
	// identity read, so a PID reaped and recycled between the poll and the signal
	// cannot receive a process-group SIGKILL.
	if !termLive(pid) {
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
