package pidutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// AliveWithCmdline answers "is this PID the process I think it is" by comparing
// argv. It short-circuited to `return true` on every non-Linux host, because
// Cmdline read only /proc/<pid>/cmdline.
//
// That turns an identity check into a bare existence check. Its callers use it
// to decide whether the PID in a poller pidfile is still *their* poller: on a
// host with high PID churn, a recycled PID owned by an unrelated live process
// then reads as "poller already running", and the caller returns success
// without starting one — cmd/gc/cmd_nudge.go returns 0, internal/session's
// submit path returns nil. Nudge and submit delivery stop for that target with
// no error and nothing logged.
//
// These tests pin the identity semantics on every platform. The existing
// coverage asserted them and then skipped off Linux, which is why the inversion
// survived.

// spawnSleeper starts a long-lived child and returns its pid. argv is exactly
// ["sleep","60"], which is what both the /proc and ps paths must report.
func spawnSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Give the exec a moment so the argv is the sleeper's, not the shell's.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if argv, err := Cmdline(cmd.Process.Pid); err == nil && len(argv) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cmd.Process.Pid
}

// TestAliveWithCmdline_RejectsLivePIDWithNonMatchingArgv is the defect, stated
// directly: a live process whose argv does NOT match must be rejected. Off Linux
// this returned true, which is what let an unrelated recycled PID pass as the
// caller's own poller.
func TestAliveWithCmdline_RejectsLivePIDWithNonMatchingArgv(t *testing.T) {
	pid := spawnSleeper(t)

	got := AliveWithCmdline(pid, func(argv []string) bool {
		return ArgvContainsSequence(argv, "definitely-not-in-this-argv")
	})

	if got {
		t.Fatalf("AliveWithCmdline(%d, non-matching) = true on %s; an unrelated live PID passes as the caller's own process", pid, runtime.GOOS)
	}
}

// TestAliveWithCmdline_AcceptsMatchingArgv is the over-correction guard: the
// check must still say yes to the real process. Passes before and after.
func TestAliveWithCmdline_AcceptsMatchingArgv(t *testing.T) {
	pid := spawnSleeper(t)

	got := AliveWithCmdline(pid, func(argv []string) bool {
		return ArgvContainsSequence(argv, "sleep", "60")
	})

	if !got {
		argv, err := Cmdline(pid)
		t.Fatalf("AliveWithCmdline(%d, matching) = false; argv=%q err=%v", pid, argv, err)
	}
}

// TestCmdline_ReturnsArgvOnThisHost is the regression test for the cause rather
// than the symptom: Cmdline must produce argv on the host it runs on. It
// returned an error on every non-Linux host, which is what forced the
// short-circuit above.
func TestCmdline_ReturnsArgvOnThisHost(t *testing.T) {
	argv, err := Cmdline(os.Getpid())
	if err != nil {
		t.Fatalf("Cmdline(self) on %s: %v", runtime.GOOS, err)
	}
	if len(argv) == 0 {
		t.Fatalf("Cmdline(self) on %s returned no argv", runtime.GOOS)
	}
}

// TestAliveWithCmdline_FalseForDeadPID and _NilMatch pin the two answers that
// must not change.
func TestAliveWithCmdline_FalseForDeadPID(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !AliveWithCmdline(pid, func([]string) bool { return true }) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("AliveWithCmdline(%d) stayed true for an exited child", pid)
}

func TestAliveWithCmdline_NilMatchIsFalse(t *testing.T) {
	if AliveWithCmdline(os.Getpid(), nil) {
		t.Fatal("AliveWithCmdline(self, nil) = true, want false")
	}
}

// TestCmdline_FailsClosedWhenUnreadable covers the direction that matters for
// safety here. An unreadable process must NOT be reported as matching: the
// caller then assumes no poller is running and starts one. A duplicate poller is
// recoverable; a silently absent one is not.
func TestCmdline_FailsClosedWhenUnreadable(t *testing.T) {
	binDir := t.TempDir()
	// A ps that produces nothing, so the non-/proc path has no argv to offer.
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	if runtime.GOOS == "linux" {
		t.Skip("on linux /proc answers directly, so the ps stub cannot make argv unreadable")
	}
	if AliveWithCmdline(os.Getpid(), func([]string) bool { return true }) {
		t.Fatal("AliveWithCmdline = true with no readable argv; must fail closed so the caller starts its poller")
	}
}

// TestPSCmdlineIsBounded mirrors the existing zombie-probe guard: a hung ps must
// not stall a caller that runs on a reconciler tick.
func TestPSCmdlineIsBounded(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))

	start := time.Now()
	_, _ = psCmdline(os.Getpid())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("psCmdline took %s, want a bounded timeout", elapsed)
	}
}
