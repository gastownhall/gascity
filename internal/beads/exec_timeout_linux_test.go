//go:build linux

package beads

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestExecTimeoutChildDiesWithHardParentExit pins that a bd subprocess
// prepared by prepareCommandForTimeout does not outlive a parent that exits
// without running any cleanup. The harness below re-execs this test binary,
// spawns `sleep` through prepareCommandForTimeout, reports its pid, and
// hard-exits; the child must be gone shortly after.
func TestExecTimeoutChildDiesWithHardParentExit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command(exe, "-test.run=^TestExecTimeoutHardExitHarness$", "--")
	cmd.Env = append(os.Environ(),
		"GC_EXEC_TIMEOUT_HARNESS=1",
		"GC_EXEC_TIMEOUT_PIDFILE="+pidFile,
	)
	out, runErr := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		t.Fatalf("run harness: %v\n%s", runErr, out)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("harness did not report a child pid (output below):\n%s\nerr: %v", out, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse pidfile %q: %v", pidBytes, err)
	}
	// Pdeathsig delivery is asynchronous; poll rather than assert at once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // never leak the reproduction
	t.Fatalf("child pid %d still alive 5s after its parent hard-exited; Pdeathsig is not set on the bd subprocess", pid)
}

// TestExecTimeoutHardExitHarness is the re-exec target for the test above.
func TestExecTimeoutHardExitHarness(t *testing.T) {
	if os.Getenv("GC_EXEC_TIMEOUT_HARNESS") != "1" {
		t.Skip("harness process")
	}
	pidFile := os.Getenv("GC_EXEC_TIMEOUT_PIDFILE")
	if pidFile == "" {
		t.Fatal("GC_EXEC_TIMEOUT_PIDFILE not set")
	}
	child := exec.Command("sleep", "300")
	prepareCommandForTimeout(child)
	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	// Hard exit: no Wait, no kill, no deferred cleanup. The child sits in its
	// own process group, so only the kernel's parent-death signal can end it.
	os.Exit(0)
}
