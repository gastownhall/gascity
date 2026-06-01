//go:build linux || darwin

package proctable

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestKillByPIDRefusesLowPIDs(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if err := KillByPID(pid); err == nil {
			t.Errorf("KillByPID(%d) succeeded, want error", pid)
		}
	}
}

func TestKillByPIDAlreadyGoneIsSuccess(t *testing.T) {
	// Spawn a short-lived process and wait for it to exit, then try to kill it.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawning test process: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	// Process is already gone. KillByPID should return nil (ESRCH → success).
	if err := KillByPID(pid); err != nil {
		t.Fatalf("KillByPID(%d) for already-dead process: %v", pid, err)
	}
}

func TestSignalPIDGroupThenFallback(t *testing.T) {
	// Spawn a child in its own process group; verify we can signal it.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	pid := cmd.Process.Pid
	// signalPID targets the process group first (-pid), then falls back to PID.
	// Both approaches should succeed for a process that's a group leader.
	if err := signalPID(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signalPID(%d, SIGTERM): %v", pid, err)
	}
}
