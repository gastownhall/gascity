package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubSupervisorHandoff models a supervisor instance that holds the
// single-instance lock for lockedProbes probes, and answers on its control
// socket from the readyAfter-th probe on (0 = never).
func stubSupervisorHandoff(t *testing.T, lockedProbes, readyAfter int32, pid int) *atomic.Int32 {
	t.Helper()
	var probes atomic.Int32
	oldAlive := supervisorAliveHook
	oldTry := tryAcquireSupervisorLock
	oldTimeout := supervisorReadyTimeout
	oldPoll := supervisorReadyPollInterval
	supervisorAliveHook = func() int {
		n := probes.Add(1)
		if readyAfter > 0 && n >= readyAfter {
			return pid
		}
		return 0
	}
	tryAcquireSupervisorLock = func() (bool, error) {
		return probes.Load() >= lockedProbes, nil
	}
	supervisorReadyTimeout = 2 * time.Second
	supervisorReadyPollInterval = time.Millisecond
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		tryAcquireSupervisorLock = oldTry
		supervisorReadyTimeout = oldTimeout
		supervisorReadyPollInterval = oldPoll
	})
	return &probes
}

// A supervisor that holds the lock while it starts up is waited for: once
// its control socket answers, the start reports that instance instead of
// failing on the held lock (ga-96smfk.85).
func TestAwaitSupervisorLockOrInstanceWaitsForAStartingInstance(t *testing.T) {
	stubSupervisorHandoff(t, 1<<30, 5, 4242)
	pid, err := awaitSupervisorLockOrInstance()
	if err != nil {
		t.Fatalf("awaitSupervisorLockOrInstance: %v", err)
	}
	if pid != 4242 {
		t.Fatalf("pid = %d, want the starting instance's 4242", pid)
	}
}

// A supervisor that holds the lock while it shuts down is waited for: once
// the lock frees, the caller may start its own instance.
func TestAwaitSupervisorLockOrInstanceWaitsForAStoppingInstance(t *testing.T) {
	probes := stubSupervisorHandoff(t, 5, 0, 0)
	pid, err := awaitSupervisorLockOrInstance()
	if err != nil {
		t.Fatalf("awaitSupervisorLockOrInstance: %v", err)
	}
	if pid != 0 {
		t.Fatalf("pid = %d, want 0 (lock free, no instance)", pid)
	}
	if probes.Load() < 5 {
		t.Fatalf("returned after %d probes, want it to wait for the lock to free", probes.Load())
	}
}

// A lock holder that neither answers nor lets go within the readiness budget
// fails the start, naming why.
func TestAwaitSupervisorLockOrInstanceTimesOut(t *testing.T) {
	stubSupervisorHandoff(t, 1<<30, 0, 0)
	supervisorReadyTimeout = 50 * time.Millisecond
	_, err := awaitSupervisorLockOrInstance()
	if err == nil {
		t.Fatal("awaitSupervisorLockOrInstance succeeded with a lock holder that never answered")
	}
	if !strings.Contains(err.Error(), "supervisor already running") || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("err = %v, want it to say a holder never answered", err)
	}
}

// An error opening the lock is not contention: fail at once.
func TestAwaitSupervisorLockOrInstanceFailsOnLockError(t *testing.T) {
	stubSupervisorHandoff(t, 1<<30, 0, 0)
	tryAcquireSupervisorLock = func() (bool, error) { return false, errors.New("opening supervisor lock: permission denied") }
	start := time.Now()
	if _, err := awaitSupervisorLockOrInstance(); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the lock error", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("waited out the readiness budget on a lock error")
	}
}

// ensureSupervisorRunning's bare start joins an instance that another start
// brought up meanwhile, rather than failing with "already running"; the
// operator-facing `gc supervisor start` still reports it as already running.
func TestBareSupervisorStartJoinsARunningInstanceOnlyWhenAsked(t *testing.T) {
	pinRealHome(t) // before the already-running check
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	stubSupervisorHandoff(t, 1<<30, 1, 4242)
	var stdout, stderr bytes.Buffer
	if code := startBareSupervisor(&stdout, &stderr, false, true); code != 0 {
		t.Fatalf("join start = %d, want 0; stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := startBareSupervisor(&stdout, &stderr, false, false); code == 0 {
		t.Fatal("gc supervisor start = 0 with an instance already running, want 1")
	}
	if !strings.Contains(stderr.String(), "supervisor already running (PID 4242)") {
		t.Fatalf("stderr = %q, want the running PID", stderr.String())
	}
}

// stoppingSupervisorPID is the PID fakeStoppingSupervisor's socket reports.
const stoppingSupervisorPID = 4242

// fakeStoppingSupervisor serves the supervisor control socket: stop answers
// ok and then done:ok, and ping answers stoppingSupervisorPID until linger
// after the stop, as a supervisor keeps its socket for a moment after done:ok.
func fakeStoppingSupervisor(t *testing.T, linger time.Duration) {
	t.Helper()
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t, "gc-run-"))
	var goneAt atomic.Int64 // UnixNano when the socket stops answering; 0 until a stop
	startTestSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), func(cmd string) string {
		switch cmd {
		case "ping":
			if gone := goneAt.Load(); gone != 0 && time.Now().UnixNano() >= gone {
				return ""
			}
			return fmt.Sprintf("%d\n", stoppingSupervisorPID)
		case "stop":
			goneAt.Store(time.Now().Add(linger).UnixNano())
			return "ok\ndone:ok\n"
		}
		return ""
	})
}

// stubSupervisorProcess models the supervisor process behind
// fakeStoppingSupervisor's socket: it is still alive (holding its
// single-instance lock) until exitAt.
func stubSupervisorProcess(t *testing.T, exitAt func() time.Time) {
	t.Helper()
	old := supervisorProcessWatch
	supervisorProcessWatch = func(pid int) func() bool {
		if pid != stoppingSupervisorPID {
			t.Errorf("watched PID %d, want the supervisor's %d", pid, stoppingSupervisorPID)
			return nil
		}
		return func() bool { return time.Now().Before(exitAt()) }
	}
	t.Cleanup(func() { supervisorProcessWatch = old })
}

// setSupervisorStopProcessExitGrace sets the least time stop --wait gives a
// supervisor process to exit once its socket is gone.
func setSupervisorStopProcessExitGrace(t *testing.T, grace time.Duration) {
	t.Helper()
	old := supervisorStopProcessExitGrace
	supervisorStopProcessExitGrace = grace
	t.Cleanup(func() { supervisorStopProcessExitGrace = old })
}

// stop --wait returns only once the supervisor process has exited, not when
// its control socket goes away: the process still holds the single-instance
// lock while it finishes shutting down (its HTTP server drains open streams
// for up to 5s), and a `gc supervisor start` right after a returned stop
// failed with "supervisor already running" (ga-96smfk.86).
func TestStopSupervisorWithWaitWaitsForTheProcessToExit(t *testing.T) {
	fakeStoppingSupervisor(t, 0)
	start := time.Now()
	exitAt := start.Add(500 * time.Millisecond)
	stubSupervisorProcess(t, func() time.Time { return exitAt })
	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, true, 10*time.Second); code != 0 {
		t.Fatalf("stop --wait = %d; stderr=%q", code, stderr.String())
	}
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Fatalf("stop --wait returned after %s, before the supervisor process exited", waited)
	}
}

// The supervisor's socket and process outlive its done:ok: it keeps the
// socket for 0.5s, then drains HTTP streams for up to 5s before it exits. So
// stop --wait gives the process that shutdown tail even when the socket wait
// ran past the --wait-timeout deadline, rather than failing on a budget the
// socket wait already spent.
func TestStopSupervisorWithWaitGivesTheProcessItsShutdownTail(t *testing.T) {
	fakeStoppingSupervisor(t, 300*time.Millisecond)
	start := time.Now()
	exitAt := start.Add(600 * time.Millisecond)
	stubSupervisorProcess(t, func() time.Time { return exitAt })
	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, true, 100*time.Millisecond); code != 0 {
		t.Fatalf("stop --wait = %d; stderr=%q", code, stderr.String())
	}
	if waited := time.Since(start); waited < 600*time.Millisecond {
		t.Fatalf("stop --wait returned after %s, before the supervisor process exited", waited)
	}
}

// A supervisor process that outlives the wait budget fails stop --wait, and
// only once the whole budget is spent: a --wait-timeout longer than the grace
// is not cut short to the grace.
func TestStopSupervisorWithWaitFailsWhenTheProcessOutlivesTheBudget(t *testing.T) {
	fakeStoppingSupervisor(t, 0)
	stubSupervisorProcess(t, func() time.Time { return time.Now().Add(time.Hour) })
	setSupervisorStopProcessExitGrace(t, 100*time.Millisecond)
	var stdout, stderr bytes.Buffer
	start := time.Now()
	if code := stopSupervisorWithWait(&stdout, &stderr, true, 300*time.Millisecond); code == 0 {
		t.Fatal("stop --wait = 0 while the supervisor process was still running")
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("stop --wait gave up after %s, before its 300ms --wait-timeout", waited)
	}
	if !strings.Contains(stderr.String(), fmt.Sprintf("PID %d to exit", stoppingSupervisorPID)) {
		t.Fatalf("stderr = %q, want the PID it waited for", stderr.String())
	}
}

// The grace also applies when the socket closes before the deadline, with less
// of it left than the grace: the process gets the grace, not just what is left
// of the --wait-timeout.
func TestStopSupervisorWithWaitGivesTheGraceBeforeTheDeadlineLapses(t *testing.T) {
	fakeStoppingSupervisor(t, 0)
	start := time.Now()
	exitAt := start.Add(600 * time.Millisecond)
	stubSupervisorProcess(t, func() time.Time { return exitAt })
	setSupervisorStopProcessExitGrace(t, 2*time.Second)
	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, true, 300*time.Millisecond); code != 0 {
		t.Fatalf("stop --wait = %d; stderr=%q", code, stderr.String())
	}
	if waited := time.Since(start); waited < 600*time.Millisecond {
		t.Fatalf("stop --wait returned after %s, before the supervisor process exited", waited)
	}
}

// A supervisor process that outlives its grace after a late socket close
// fails stop --wait, which reports how long it waited for the process, not
// what was left of a deadline the socket wait had already spent.
func TestStopSupervisorWithWaitReportsHowLongItWaitedForTheProcess(t *testing.T) {
	fakeStoppingSupervisor(t, 300*time.Millisecond)
	stubSupervisorProcess(t, func() time.Time { return time.Now().Add(time.Hour) })
	setSupervisorStopProcessExitGrace(t, 200*time.Millisecond)
	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, true, 100*time.Millisecond); code == 0 {
		t.Fatal("stop --wait = 0 while the supervisor process was still running")
	}
	m := regexp.MustCompile(fmt.Sprintf(`timed out after (\S+) waiting for supervisor PID %d to exit`, stoppingSupervisorPID)).FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("stderr = %q, want the PID it waited for", stderr.String())
	}
	if waited, err := time.ParseDuration(m[1]); err != nil || waited < 200*time.Millisecond {
		t.Fatalf("stop --wait reported waiting %s for the process (%v), want at least its 200ms grace", m[1], err)
	}
}

// Only a `gc supervisor run` process is watched: a PID the socket reports
// that is not one (another PID namespace, a recycled PID) is not waited for.
func TestSupervisorProcessWatchIgnoresANonSupervisorPID(t *testing.T) {
	if alive := supervisorProcessWatch(os.Getpid()); alive != nil {
		t.Fatal("watched the test process as if it were a supervisor")
	}
	if alive := supervisorProcessWatch(0); alive != nil {
		t.Fatal("watched PID 0")
	}
}
