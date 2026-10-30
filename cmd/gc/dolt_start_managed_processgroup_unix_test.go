//go:build unix

package main

// Terminating a managed dolt server by negative PID addresses a POSIX
// process group. Windows has no such addressing -- processgroup.Terminate
// refuses a bare PID outright -- and managed dolt is gated off there.
import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// TestTerminateManagedDoltStartedProcessSkipsReusedPIDBeforeSIGKILL is the
// PR #4004 attempt-4 regression: the entry identity guard is not sufficient on
// its own. terminateManagedDoltPID SIGTERMs, waits out the grace, then escalates
// to SIGKILL by bare PID, so a child that exits and has its PID reused *during*
// the grace must be re-verified before the forced kill. This drives the real
// cleanup with an identity that matches at the SIGTERM check and mismatches at
// the SIGKILL re-check, against a process that ignores SIGTERM so the escalation
// is actually reached.
func TestTerminateManagedDoltStartedProcessSkipsReusedPIDBeforeSIGKILL(t *testing.T) {
	city, _ := raceTestCity(t, "[workspace]\nname = \"reuse-test\"\n\n[daemon]\ndolt_stop_timeout = \"100ms\"\n")
	dataDir := filepath.Join(city, ".beads", "dolt")
	pid := startSigtermIgnoringProcess(t, dataDir)

	// The snapshot is ticks=1111. The live PID re-reads as 1111 on the first
	// check (before SIGTERM: still our child) then 2222 afterwards (before the
	// SIGKILL escalation: our child exited and the PID was reused mid-grace).
	var reads int
	oldTicks := managedDoltTestReadStartTimeTicks
	managedDoltTestReadStartTimeTicks = func(int) uint64 {
		reads++
		if reads == 1 {
			return 1111
		}
		return 2222
	}
	t.Cleanup(func() { managedDoltTestReadStartTimeTicks = oldTicks })

	oldLog := managedDoltCleanupLogf
	var logged []string
	managedDoltCleanupLogf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { managedDoltCleanupLogf = oldLog })

	terminateManagedDoltStartedProcess(managedDoltStartedProcess{CityPath: city, PID: pid, StartTimeTicks: 1111})

	if reads < 2 {
		t.Fatalf("identity re-read %d time(s); the SIGKILL escalation must re-verify identity (want >= 2)", reads)
	}
	if !pidAlive(pid) {
		t.Fatal("startup cleanup SIGKILLed a PID reused during the SIGTERM grace; escalation guard not enforced")
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "SIGKILL") || !strings.Contains(logged[0], strconv.Itoa(pid)) {
		t.Fatalf("expected exactly one SIGKILL-skip log naming pid %d, got %v", pid, logged)
	}
}

// TestTerminateManagedDoltStartedProcessForceKillsWhenIdentityStable is the
// happy-path companion: when the start identity stays stable across the whole
// termination, the SIGKILL escalation must still fire on a process that ignores
// SIGTERM. This proves the escalation guard does not break the legacy forced
// kill for our own wedged child.
func TestTerminateManagedDoltStartedProcessForceKillsWhenIdentityStable(t *testing.T) {
	city, _ := raceTestCity(t, "[workspace]\nname = \"stable-test\"\n\n[daemon]\ndolt_stop_timeout = \"100ms\"\n")
	dataDir := filepath.Join(city, ".beads", "dolt")
	pid := startSigtermIgnoringProcess(t, dataDir)

	oldTicks := managedDoltTestReadStartTimeTicks
	managedDoltTestReadStartTimeTicks = func(int) uint64 { return 3333 }
	t.Cleanup(func() { managedDoltTestReadStartTimeTicks = oldTicks })

	terminateManagedDoltStartedProcess(managedDoltStartedProcess{CityPath: city, PID: pid, StartTimeTicks: 3333})

	deadline := time.Now().Add(2 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatal("startup cleanup did not SIGKILL a stable-identity child that ignored SIGTERM")
	}
}

// TestTerminateManagedDoltTestPIDKillsProcessGroup is the #2313 follow-up M3
// regression: when the target is a process-group leader, terminate must
// signal the whole group so descendant dolt workers do not survive.
// Demonstration: spawn a shell as group leader, fork a backgrounded sleep
// child, call terminateManagedDoltTestPID on the shell. Both must die.
// Without the M3 fix (leader-only kill), the child outlives the shell.
func TestTerminateManagedDoltTestPIDKillsProcessGroup(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child.pid")
	// Shell becomes the new process group leader (Setpgid:true). It forks
	// a backgrounded sleep that inherits that group, records the child's
	// PID, then waits.
	cmd := exec.Command("/bin/sh", "-c", `sleep 90 & echo $! > "$1"; wait`, "sh", childFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shell: %v", err)
	}
	shellPID := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Wait for the child PID to be recorded.
	var childPID int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(childFile)
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				childPID = pid
				break
			}
		}
	}
	if childPID == 0 {
		t.Fatalf("child sleep never recorded its PID at %s", childFile)
	}

	if err := terminateManagedDoltTestPID(shellPID); err != nil {
		t.Fatalf("terminateManagedDoltTestPID(%d): %v", shellPID, err)
	}

	// Allow a short window for the kernel to mark both pids dead.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(shellPID) && !pidAlive(childPID) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(shellPID) {
		t.Errorf("shell pid %d still alive after pgid terminate", shellPID)
	}
	if pidAlive(childPID) {
		t.Errorf("child pid %d still alive after pgid terminate; M3 pgid-kill regression", childPID)
	}
}

// TestTerminateManagedDoltTestPIDLeaderOnlyForNonGroupLeader asserts the
// safety guard added in M3: when the target is NOT its own pgid leader (e.g.
// the watchdog inheriting the test binary's group), terminate must NOT
// signal the whole group — that would take down the test binary. We pick a
// child of the test binary that did NOT call Setpgid; it inherits the test
// binary's group. Terminate must only kill the child.
func TestTerminateManagedDoltTestPIDLeaderOnlyForNonGroupLeader(t *testing.T) {
	// Spawn a sleep WITHOUT Setpgid — it inherits the test binary's pgid.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", pid, err)
	}
	if pgid == pid {
		t.Skip("sleep happens to be its own group leader; cannot exercise leader-only fallback")
	}

	if err := terminateManagedDoltTestPID(pid); err != nil {
		t.Fatalf("terminateManagedDoltTestPID(%d): %v", pid, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Errorf("sleep pid %d still alive after terminate", pid)
	}
	// Sanity: the test binary itself is still alive (we did not pgid-kill
	// our own group). If we had, the test process would have died and this
	// assertion would never run — but if it did, this guards against a
	// future regression where the fallback path forgets the leader check.
	if !pidAlive(os.Getpid()) {
		t.Fatalf("test binary signaled by terminate fallback; pgid safety check failed")
	}
}
