package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// fakeStoppingSupervisor serves the supervisor control socket: ping answers
// pid until a stop has finished, stop answers ok and then done:ok.
func fakeStoppingSupervisor(t *testing.T, pid int) {
	t.Helper()
	gcHome := shortTempDir(t, "gc-home-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", shortTempDir(t, "gc-run-"))
	var stopped atomic.Bool
	startTestSupervisorSocket(t, filepath.Join(gcHome, "supervisor.sock"), func(cmd string) string {
		switch cmd {
		case "ping":
			if stopped.Load() {
				return ""
			}
			return fmt.Sprintf("%d\n", pid)
		case "stop":
			stopped.Store(true)
			return "ok\ndone:ok\n"
		}
		return ""
	})
}

// stubSupervisorProcess models the supervisor process behind the socket: it
// is still alive (holding its single-instance lock) until exitAt.
func stubSupervisorProcess(t *testing.T, wantPID int, exitAt func() time.Time) {
	t.Helper()
	old := supervisorProcessWatch
	supervisorProcessWatch = func(pid int) func() bool {
		if pid != wantPID {
			t.Errorf("watched PID %d, want the supervisor's %d", pid, wantPID)
			return nil
		}
		return func() bool { return time.Now().Before(exitAt()) }
	}
	t.Cleanup(func() { supervisorProcessWatch = old })
}

// stop --wait returns only once the supervisor process has exited, not when
// its control socket goes away: the process still holds the single-instance
// lock while it finishes shutting down (its HTTP server drains open streams
// for up to 5s), and a `gc supervisor start` right after a returned stop
// failed with "supervisor already running" (ga-96smfk.86).
func TestStopSupervisorWithWaitWaitsForTheProcessToExit(t *testing.T) {
	fakeStoppingSupervisor(t, 4242)
	var exitAt atomic.Value
	exitAt.Store(time.Now().Add(time.Hour))
	stubSupervisorProcess(t, 4242, func() time.Time { return exitAt.Load().(time.Time) })
	go func() {
		time.Sleep(100 * time.Millisecond)
		exitAt.Store(time.Now().Add(400 * time.Millisecond))
	}()
	start := time.Now()
	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, true, 10*time.Second); code != 0 {
		t.Fatalf("stop --wait = %d; stderr=%q", code, stderr.String())
	}
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Fatalf("stop --wait returned after %s, before the supervisor process exited", waited)
	}
}

// A supervisor process that outlives the wait budget fails stop --wait.
func TestStopSupervisorWithWaitFailsWhenTheProcessOutlivesTheBudget(t *testing.T) {
	fakeStoppingSupervisor(t, 4242)
	stubSupervisorProcess(t, 4242, func() time.Time { return time.Now().Add(time.Hour) })
	var stdout, stderr bytes.Buffer
	if code := stopSupervisorWithWait(&stdout, &stderr, true, 300*time.Millisecond); code == 0 {
		t.Fatal("stop --wait = 0 while the supervisor process was still running")
	}
	if !strings.Contains(stderr.String(), "PID 4242 to exit") {
		t.Fatalf("stderr = %q, want the PID it waited for", stderr.String())
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
