package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// descendantPIDAfterTimeout runs a shell command that forks a long-lived child,
// records that child's PID, and then blocks past the supplied timeout. It
// returns the child's PID once the timed-out command has returned.
//
// The command is the shape that matters: a hook whose `sh -c` spawns work that
// outlives the shell (the real one is a bd | awk | xargs pipeline).
func descendantPIDAfterTimeout(t *testing.T, timeout time.Duration, prepare func(*exec.Cmd)) int {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The child sleeps well past every deadline in this test, so "is it alive"
	// is a question about the kill and never about the child finishing early.
	command := "sleep 5 & echo $! > " + pidFile + "; sleep 5"

	if _, err := runShellCommand(command, "", timeout, nil, prepare); err == nil {
		t.Fatal("command returned without error; it was supposed to hit its timeout")
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("child never recorded its PID (%v); the fixture did not reach the fork", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("recorded child PID %q is not usable: %v", raw, err)
	}
	return pid
}

// waitForProcessExit polls until pid is gone or the deadline passes, so the
// assertion does not race the SIGTERM-then-SIGKILL escalation.
func waitForProcessExit(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHookTimeoutKillsTheWholeProcessGroup pins the containment the on_boot
// bound depends on.
//
// A timed-out hook is canceled by CommandContext, which kills the `sh` it
// started and, after WaitDelay, stops waiting on the pipe. It does NOT kill the
// shell's descendants -- WaitDelay closes I/O, it does not signal a process
// tree. So a hook that timed out mid-bd left that bd running and holding a
// store connection, while its freed semaphore slot admitted the next hook: the
// bound reads as 6 while the store sees more, which is the read storm the bound
// exists to prevent. Starting the hook as its own process-group leader and
// terminating the GROUP on cancellation is what makes releasing the slot mean
// the work actually stopped.
//
// The control is the point of this test. The same fixture WITHOUT the group
// cleanup must leave the descendant running; without that row, "the child is
// gone" would also pass on a machine that reaped it for an unrelated reason.
func TestHookTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	t.Run("group cleanup reaps the descendant", func(t *testing.T) {
		pid := descendantPIDAfterTimeout(t, 300*time.Millisecond, hookProcessGroupCleanup)
		if !waitForProcessExit(pid, 10*time.Second) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the hook's descendant survived the timeout; a freed pool slot does not mean the work stopped")
		}
	})

	t.Run("control: without it the descendant survives", func(t *testing.T) {
		pid := descendantPIDAfterTimeout(t, 300*time.Millisecond, nil)
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		if waitForProcessExit(pid, 2*time.Second) {
			t.Fatal("the descendant died without the group cleanup, so the row above proves nothing about the cleanup")
		}
	})
}

// shellPIDAndGroup runs a probe through runner and returns the shell's own PID
// and its process-group ID. A shell that leads its own group reports the two as
// equal; one that inherited gc's group does not.
func shellPIDAndGroup(t *testing.T, runner func() (string, error)) (pid, pgid int) {
	t.Helper()
	out, err := runner()
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		t.Fatalf("probe output = %q, want the shell's pid and pgid", out)
	}
	pid, err = strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("probe pid %q: %v", fields[0], err)
	}
	pgid, err = strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("probe pgid %q: %v", fields[1], err)
	}
	return pid, pgid
}

// pgidProbe asks the shell for its own pid and process-group id.
const pgidProbe = `echo $$ $(ps -o pgid= -p $$)`

// TestShellRunHookLeadsItsOwnProcessGroup proves the production hook runner --
// not just the helper -- puts hooks in their own group, so the containment
// cannot be silently unwired by a later edit to shellRunHook.
//
// The control is shellScaleCheck, which deliberately keeps the old behavior:
// its probes run on the reconciler tick under a different bound and timeout,
// and changing their signal semantics is not this change's business.
func TestShellRunHookLeadsItsOwnProcessGroup(t *testing.T) {
	pid, pgid := shellPIDAndGroup(t, func() (string, error) {
		return shellRunHook(pgidProbe, "", nil)
	})
	if pid != pgid {
		t.Fatalf("hook shell pid %d is in group %d, want to lead its own group; without that there is no group to terminate on timeout", pid, pgid)
	}

	probePID, probePGID := shellPIDAndGroup(t, func() (string, error) {
		return shellCommand(pgidProbe, "", 30*time.Second, nil)
	})
	if probePID == probePGID {
		t.Fatalf("the scale_check probe shell also leads its own group (pid %d), so the assertion above does not distinguish the hook path", probePID)
	}
}
