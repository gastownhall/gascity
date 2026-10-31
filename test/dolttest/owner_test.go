package dolttest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// ownerHelperEnv re-executes this test binary as an armed owner that spawns a
// detached grandchild, the shape of bd spawning its Setsid db-proxy-child.
const ownerHelperEnv = "DOLTTEST_OWNER_HELPER"

// ownerReapWait bounds how long a test waits for a reap it expects.
const ownerReapWait = 15 * time.Second

// newTestProcess builds the command for argv with env (nil inherits this
// process's), in a new session when setsid is true. Once Start returns, the
// exec has happened, so the child's /proc environ is already its own.
func newTestProcess(argv, env []string, setsid bool) *exec.Cmd {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	if setsid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	return cmd
}

// runOwnerHelper registers a run directory for owner-exit removal, starts a
// detached `sleep`, prints "<sleep pid> <run dir>", and exits normally once
// its stdin closes, unless it is killed first. It never waits on the sleep:
// like bd's proxy child, the grandchild is meant to outlive it.
func runOwnerHelper() int {
	runDir, err := os.MkdirTemp("", "dolttest-owner-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		return 1
	}
	if err := RemoveOnOwnerExit(runDir); err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		return 1
	}
	sleep := newTestProcess([]string{"sleep", "300"}, nil, true)
	if err := sleep.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err) //nolint:errcheck
		return 1
	}
	fmt.Printf("%d %s\n", sleep.Process.Pid, runDir) //nolint:errcheck
	_ = sleep.Process.Release()
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func requireProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("owner reaper needs /proc")
	}
}

// requireExit fails the test unless the process exits within ownerReapWait.
func requireExit(t *testing.T, what string, pid int, startTime string) {
	t.Helper()
	if !waitProcessExit(pid, startTime, ownerReapWait) {
		t.Fatalf("%s pid %d still alive after %s", what, pid, ownerReapWait)
	}
}

// findOwnerWatchdog returns the pid and start time of the watchdog guarding
// the run whose OwnerEnv token is owner.
func findOwnerWatchdog(t *testing.T, owner string) (int, string) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || procEnvValue(pid, ownerWatchdogEnv) != owner {
			continue
		}
		startTime, err := pidutil.StartTime(pid)
		if err != nil {
			continue
		}
		return pid, startTime
	}
	t.Fatalf("no owner watchdog for %s", owner)
	return 0, ""
}

// TestOwnerReaperReapsDetachedGrandchildWhenOwnerDies starts an armed owner
// that leaves a detached grandchild and a registered run directory behind,
// ends the owner, and requires the owner's watchdog to reap both. SIGKILL is
// the case no in-process cleanup can handle: it is what a gate sends, and
// what a timed-out run amounts to for its t.Cleanup.
func TestOwnerReaperReapsDetachedGrandchildWhenOwnerDies(t *testing.T) {
	requireProc(t)
	for _, tc := range []struct {
		name string
		// end ends the helper, given the write end of the helper's stdin.
		end func(helper *exec.Cmd, stdin *os.File) error
	}{
		{name: "sigkill", end: func(helper *exec.Cmd, _ *os.File) error { return helper.Process.Kill() }},
		{name: "normal exit", end: func(_ *exec.Cmd, stdin *os.File) error { return stdin.Close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold, release, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer release.Close() //nolint:errcheck
			helper := newTestProcess([]string{os.Args[0]}, append(os.Environ(), ownerHelperEnv+"=1"), false)
			helper.Stdin = hold
			helper.Stderr = os.Stderr
			stdout, err := helper.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			_ = hold.Close()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil {
				_ = helper.Process.Kill()
				_ = helper.Wait()
				t.Fatalf("reading report from owner helper: %v", err)
			}
			pidField, runDir, ok := strings.Cut(strings.TrimSpace(line), " ")
			grandchild, err := strconv.Atoi(pidField)
			if !ok || err != nil {
				t.Fatalf("parsing owner helper report %q", line)
			}
			t.Cleanup(func() {
				_ = syscall.Kill(grandchild, syscall.SIGKILL)
				_ = os.RemoveAll(runDir)
			})
			grandchildStart, err := pidutil.StartTime(grandchild)
			if err != nil {
				t.Fatalf("reading grandchild start time: %v", err)
			}
			owner, err := ownerToken(helper.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			if got := procEnvValue(grandchild, OwnerEnv); got != owner {
				t.Fatalf("grandchild %s = %q, want the helper's token %q", OwnerEnv, got, owner)
			}
			watchdog, watchdogStart := findOwnerWatchdog(t, owner)

			if err := tc.end(helper, release); err != nil {
				t.Fatal(err)
			}
			_ = helper.Wait()

			requireExit(t, "owner watchdog", watchdog, watchdogStart)
			if pidutil.AliveWithStartTime(grandchild, grandchildStart) {
				t.Errorf("detached grandchild pid %d survived its owner's watchdog", grandchild)
			}
			if _, err := os.Stat(runDir); !os.IsNotExist(err) {
				t.Errorf("registered run dir %s survived its owner's watchdog (stat err %v)", runDir, err)
			}
		})
	}
}

// TestOwnedProcessSweeps checks which marked processes each sweep reaps.
func TestOwnedProcessSweeps(t *testing.T) {
	requireProc(t)
	type marked struct {
		pid       int
		startTime string
	}
	startMarked := func(t *testing.T, owner string) marked {
		t.Helper()
		cmd := newTestProcess([]string{"sleep", "300"}, append(envWithout(os.Environ(), OwnerEnv), OwnerEnv+"="+owner), false)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = cmd.Wait() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		startTime, err := pidutil.StartTime(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		return marked{pid: cmd.Process.Pid, startTime: startTime}
	}

	t.Run("stale sweep reaps only orphans of dead owners", func(t *testing.T) {
		liveOwner, err := ownerToken(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		// This pid with a start time it never had names a process that is
		// gone: the identity a dead run leaves in its orphans' environments.
		deadOwner := strconv.Itoa(os.Getpid()) + ".0"
		orphan := startMarked(t, deadOwner)
		live := startMarked(t, liveOwner)
		malformed := startMarked(t, "not-a-token")

		reaped := sweepStaleOwned()

		found := false
		for _, p := range reaped {
			found = found || p.pid == orphan.pid
		}
		if !found {
			t.Errorf("sweepStaleOwned() = %v, want it to include orphan pid %d", reaped, orphan.pid)
		}
		requireExit(t, "orphan", orphan.pid, orphan.startTime)
		if !pidutil.AliveWithStartTime(live.pid, live.startTime) {
			t.Errorf("sweep reaped pid %d owned by a live run", live.pid)
		}
		if !pidutil.AliveWithStartTime(malformed.pid, malformed.startTime) {
			t.Errorf("sweep reaped pid %d whose marker names no process", malformed.pid)
		}
	})

	t.Run("reap spares other owners", func(t *testing.T) {
		mine := startMarked(t, "4242.17")
		other := startMarked(t, "4243.17")

		reapOwned("4242.17")

		requireExit(t, "process carrying the reaped token", mine.pid, mine.startTime)
		if !pidutil.AliveWithStartTime(other.pid, other.startTime) {
			t.Errorf("reapOwned killed pid %d owned by another token", other.pid)
		}
	})
}

func TestRemoveOnOwnerExitRejectsUnsafePaths(t *testing.T) {
	requireProc(t)
	for _, dir := range []string{"relative/dir", "/tmp/two\nlines"} {
		if err := RemoveOnOwnerExit(dir); err == nil {
			t.Errorf("RemoveOnOwnerExit(%q) = nil, want an error", dir)
		}
	}
}
