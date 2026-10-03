//go:build !windows

package execgrace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestApplyTrapRunsBeforeKill is the regression test for the staged-content
// data-loss class: a setup script that has moved files aside and registered a
// rollback trap must get to run that trap when its deadline expires. With
// Go's default context-cancel (SIGKILL) the trap can never run; with Apply the
// group interrupt reaches the shell and the trap restores state before the
// grace escalation.
func TestApplyTrapRunsBeforeKill(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "restored")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// The trap models worktree-setup.sh's restore_stage: it must observe the
	// interrupt and write the marker (i.e. "move the staged files back").
	script := `trap 'echo restored > "$MARKER"; exit 130' INT TERM; sleep 30`
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Env = append(os.Environ(), "MARKER="+marker)
	result := Apply(cmd, 5*time.Second)

	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("rollback trap never ran — staged state would have been lost: %v", err)
	}
	if outcome := result.Outcome(); outcome != CancelGroupSignaled {
		t.Fatalf("expected CancelGroupSignaled, got %v", outcome)
	}
}

// TestApplyForceKillsUncooperative proves the grace escalation: a command that
// ignores the interrupt must still die within WaitDelay rather than hanging
// the caller forever.
func TestApplyForceKillsUncooperative(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", `trap '' INT TERM; sleep 30`)
	result := Apply(cmd, 1*time.Second)

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	// Deadline (200ms) + grace (1s) + slack. Well under sleep 30.
	if elapsed > 10*time.Second {
		t.Fatalf("uncooperative command outlived the grace escalation: %v", elapsed)
	}
	// The group signal is still delivered successfully here — the ignoring
	// process just doesn't act on it. That makes this CancelGroupSignaled,
	// not CancelForceKilled: os/exec's own WaitDelay escalation (SIGKILL) is
	// what actually ends the process, running independently of — and after —
	// our cmd.Cancel closure, which already reported delivery. CancelForceKilled
	// is reserved for when interruptProcessGroup itself fails to deliver and
	// *our* fallback cmd.Process.Kill() is what fires (see
	// TestApplyForceKilledWhenGroupSignalFails).
	if outcome := result.Outcome(); outcome != CancelGroupSignaled {
		t.Fatalf("expected CancelGroupSignaled (signal delivered; os/exec's own WaitDelay kill finishes the job), got %v", outcome)
	}
}

// TestApplyAcceptedFlag proves the delivered-cancellation flag contract that
// internal/runtime/exec's cancellation-wins error mapping depends on.
func TestApplyAcceptedFlag(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", `sleep 30`)
	result := Apply(cmd, 2*time.Second)
	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if !result.Delivered() {
		t.Fatal("accepted flag must record the delivered cancellation")
	}

	// A command that finishes on its own must not set the flag. (Cancel
	// requires a context-created command even when the context never fires.)
	cmd2 := exec.CommandContext(context.Background(), "sh", "-c", "true")
	result2 := Apply(cmd2, 2*time.Second)
	if err := cmd2.Run(); err != nil {
		t.Fatalf("healthy command failed: %v", err)
	}
	if result2.Delivered() {
		t.Fatal("accepted flag must stay false when the command completes normally")
	}
}

// TestApplyLeaderSignaledOnlyWhenGetpgidFails proves the leader-only signal
// fallback: when the process group cannot be resolved, Apply must still
// interrupt the process leader directly, and record that no group signal
// was ever attempted.
//
// Not parallel: overrides the package-level getpgid seam.
func TestApplyLeaderSignaledOnlyWhenGetpgidFails(t *testing.T) {
	origGetpgid := getpgid
	getpgid = func(_ int) (int, error) {
		return 0, syscall.EINVAL
	}
	defer func() { getpgid = origGetpgid }()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sleep", "30")
	result := Apply(cmd, 2*time.Second)

	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if !result.Delivered() {
		t.Fatal("accepted flag must record the delivered cancellation")
	}
	if outcome := result.Outcome(); outcome != CancelLeaderSignaledOnly {
		t.Fatalf("expected CancelLeaderSignaledOnly, got %v", outcome)
	}
}

// TestApplyForceKilledWhenGroupSignalFails proves the last-resort fallback:
// when the process-group signal itself fails outright (not just "already
// gone"), Apply must fall back to killing the leader directly rather than
// leaving the command to run out the clock on WaitDelay.
//
// Not parallel: overrides the package-level killProcessGroup seam.
func TestApplyForceKilledWhenGroupSignalFails(t *testing.T) {
	origKill := killProcessGroup
	killProcessGroup = func(_ int, _ syscall.Signal) error {
		return syscall.EPERM
	}
	defer func() { killProcessGroup = origKill }()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sleep", "30")
	result := Apply(cmd, 2*time.Second)

	if err := cmd.Run(); err == nil {
		t.Fatal("expected the canceled command to report an error")
	}
	if !result.Delivered() {
		t.Fatal("accepted flag must record the delivered cancellation")
	}
	if outcome := result.Outcome(); outcome != CancelForceKilled {
		t.Fatalf("expected CancelForceKilled, got %v", outcome)
	}

	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("expected a syscall.WaitStatus, got %T", cmd.ProcessState.Sys())
	}
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("expected the process to be killed by SIGKILL, got signaled=%v signal=%v", status.Signaled(), status.Signal())
	}
}
