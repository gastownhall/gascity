//go:build !windows

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSignalProcessGroupUsesRecordedGroupOrTheProcess(t *testing.T) {
	var got []int
	restore := swapProcessGroupKill(func(pid int, _ syscall.Signal) error {
		got = append(got, pid)
		return nil
	})
	t.Cleanup(restore)

	cmd := &exec.Cmd{Process: &os.Process{Pid: 4242}}
	if err := SignalProcessGroup(cmd, 0, syscall.SIGTERM); err != nil {
		t.Fatalf("direct signal: %v", err)
	}
	if err := SignalProcessGroup(cmd, 5150, syscall.SIGTERM); err != nil {
		t.Fatalf("group signal: %v", err)
	}
	if err := SignalProcessGroup(nil, 0, syscall.SIGTERM); err != nil {
		t.Fatalf("nil command: %v", err)
	}
	if len(got) != 2 || got[0] != 4242 || got[1] != -5150 {
		t.Fatalf("kill targets = %v, want [4242 -5150]", got)
	}
}

// TestTerminateManagedProcessSignalsRecordedGroupAfterLeaderExit locks the
// case a Getpgid check gets wrong: the leader has been reaped, Getpgid
// returns ESRCH, and a descendant still holds the group. The id recorded at
// the Setpgid spawn still names that group, so termination must signal it.
func TestTerminateManagedProcessSignalsRecordedGroupAfterLeaderExit(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! > \"$1\"; exit 0", "sh", pidFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait leader: %v", err)
	}
	if _, err := syscall.Getpgid(pgid); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Getpgid(reaped leader) = %v, want ESRCH", err)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("group %d should still be addressable: %v", pgid, err)
	}
	child := readPIDFile(t, pidFile)
	if child == pgid {
		t.Fatalf("descendant pid %d is the reaped leader", child)
	}

	calls := make(chan int, 4)
	restore := swapProcessGroupKill(func(pid int, sig syscall.Signal) error {
		err := syscall.Kill(pid, sig)
		calls <- pid
		return err
	})
	defer restore()

	done := make(chan struct{})
	finish := func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}
	t.Cleanup(finish)

	errCh := make(chan error, 1)
	go func() {
		errCh <- TerminateManagedProcess(cmd, pgid, done, time.Hour)
	}()

	target := waitKillTarget(t, calls)
	if target != -pgid {
		t.Fatalf("kill target = %d, want recorded group %d", target, -pgid)
	}
	waitPIDGone(t, child)
	finish()

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("TerminateManagedProcess: %v", err)
		}
	case <-timer.C:
		t.Fatal("TerminateManagedProcess did not return after the process exited")
	}
}

func waitKillTarget(t *testing.T, calls <-chan int) int {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case pid := <-calls:
		return pid
	case <-timer.C:
		t.Fatal("timed out waiting for the recorded-group signal")
	}
	return 0
}

func swapProcessGroupKill(fn func(int, syscall.Signal) error) func() {
	prev := processGroupKill
	processGroupKill = fn
	return func() { processGroupKill = prev }
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil || pid <= 1 {
		t.Fatalf("child pid %q: %v", data, err)
	}
	return pid
}

func waitPIDGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("pid %d still alive (%v)", pid, err)
		case <-ticker.C:
		}
	}
}
