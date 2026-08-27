package runtime

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// captureSignalKill swaps the kill(2) indirection for a recorder so a test
// can assert which target SignalProcessGroup chose without signaling
// anything real.
func captureSignalKill(t *testing.T, targets *[]int) {
	t.Helper()
	previous := signalKill
	signalKill = func(pid int, _ syscall.Signal) error {
		*targets = append(*targets, pid)
		return nil
	}
	t.Cleanup(func() { signalKill = previous })
}

// startManagedForTest starts a long-lived child, optionally as the leader of
// its own process group the way every provider spawn path does.
func startManagedForTest(t *testing.T, ownGroup bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start managed process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// TestSignalProcessGroupUsesGroupForGroupLeader keeps the signal wide enough
// for the case it exists to serve: a provider-managed child spawned with
// Setpgid leads its own group, so signaling the group takes its descendants
// (an agent CLI's own subprocesses) with it.
func TestSignalProcessGroupUsesGroupForGroupLeader(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := startManagedForTest(t, true)
	var targets []int
	captureSignalKill(t, &targets)

	if err := SignalProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("SignalProcessGroup() error = %v, want nil", err)
	}
	want := -cmd.Process.Pid
	if len(targets) != 1 || targets[0] != want {
		t.Fatalf("SignalProcessGroup targets = %v, want [%d] (the group it leads)", targets, want)
	}
}

// TestSignalProcessGroupNeverGroupSignalsANonLeader is the ga-8qmy
// regression guard. The doc contract already promises a per-process fallback
// "for older sessions ... that cannot signal by group", but inferring that
// case from a failed kill(-pid) never works: kill(-pid) names the group whose
// id equals pid, so for a child that did not become a group leader it
// resolves to an unrelated live tree and *succeeds*. The fallback is then
// unreachable exactly when it is needed, and a stranger's processes take the
// signal with nothing logged.
func TestSignalProcessGroupNeverGroupSignalsANonLeader(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := startManagedForTest(t, false)
	var targets []int
	captureSignalKill(t, &targets)

	if err := SignalProcessGroup(cmd, syscall.SIGTERM); err != nil {
		t.Fatalf("SignalProcessGroup() error = %v, want nil", err)
	}
	for _, target := range targets {
		if target < 0 {
			t.Fatalf("SignalProcessGroup signaled group %d for pid %d, which does not lead a group; that group belongs to an unrelated process tree",
				target, cmd.Process.Pid)
		}
	}
	// The group path must not be entered at all: the per-process fallback
	// goes through os.Process.Signal, not the kill(2) indirection.
	if len(targets) != 0 {
		t.Fatalf("SignalProcessGroup took the kill(2) group path (targets = %v) for a non-leader; want the per-process fallback", targets)
	}
}

// TestSignalProcessGroupTerminatesANonLeader pins the behavior the
// narrowing must not cost: a managed child that never became a group leader
// is still signaled, just directly.
func TestSignalProcessGroupTerminatesANonLeader(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start managed process: %v", err)
	}

	if err := SignalProcessGroup(cmd, syscall.SIGKILL); err != nil {
		t.Fatalf("SignalProcessGroup() error = %v, want nil", err)
	}

	err := cmd.Wait()
	exitErr := &exec.ExitError{}
	ok := errors.As(err, &exitErr)
	if !ok {
		t.Fatalf("wait error = %v, want a signal exit", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("wait status type = %T, want syscall.WaitStatus", exitErr.Sys())
	}
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("managed process exited with %v, want death by SIGKILL", exitErr)
	}
}

// TestSignalProcessGroupIgnoresUnstartedCommands keeps the nil guards intact.
func TestSignalProcessGroupIgnoresUnstartedCommands(t *testing.T) {
	if err := SignalProcessGroup(nil, syscall.SIGTERM); err != nil {
		t.Fatalf("SignalProcessGroup(nil) = %v, want nil", err)
	}
	if err := SignalProcessGroup(exec.Command("sleep", "1"), syscall.SIGTERM); err != nil {
		t.Fatalf("SignalProcessGroup(unstarted) = %v, want nil", err)
	}
}
