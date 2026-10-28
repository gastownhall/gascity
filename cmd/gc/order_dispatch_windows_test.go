//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// TestShellExecRunnerRunsPowerShellOnWindows pins the order runner on
// Windows: the command runs through PowerShell, its output and exit status
// come back, and a command whose program is a POSIX shell script is refused
// without starting anything.
func TestShellExecRunnerRunsPowerShellOnWindows(t *testing.T) {
	ctx := context.Background()
	if out, err := shellExecRunner(ctx, "if ($true) { [Console]::Out.Write('ran') }", t.TempDir(), nil); err != nil || string(out) != "ran" {
		t.Fatalf("shellExecRunner = %q, %v; want \"ran\", nil", out, err)
	}
	_, err := shellExecRunner(ctx, "cmd.exe /c exit 4", t.TempDir(), nil)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 4 {
		t.Fatalf("shellExecRunner(exit 4) error = %v, want exit status 4", err)
	}
	out, err := shellExecRunner(ctx, "$PACK_DIR/assets/scripts/gate-sweep.sh", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "order command refused") || len(out) != 0 {
		t.Fatalf("shellExecRunner(.sh) = %q, %v; want a refusal and no output", out, err)
	}
}

// TestOrderDispatcherLeavesRefusedExecOrdersUnscheduled pins that Windows
// schedules no exec order it refuses to run, and names each one once.
func TestOrderDispatcherLeavesRefusedExecOrdersUnscheduled(t *testing.T) {
	set := []orders.Order{
		{Name: "gate-sweep", Trigger: "cooldown", Interval: "30s", Exec: "$PACK_DIR/assets/scripts/gate-sweep.sh"},
		{Name: "beads-health", Trigger: "cooldown", Interval: "30s", Exec: "gc beads health --quiet"},
	}
	var stderr bytes.Buffer
	d := buildOrderDispatcherFromOrderSet(nil, t.TempDir(), &config.City{}, set, events.Discard, &stderr)
	md, ok := d.(*memoryOrderDispatcher)
	if !ok {
		t.Fatalf("dispatcher = %T, want *memoryOrderDispatcher; stderr: %s", d, stderr.String())
	}
	var names []string
	for _, a := range md.aa {
		names = append(names, a.Name)
	}
	if strings.Join(names, " ") != "beads-health" {
		t.Errorf("scheduled %q, want only beads-health", names)
	}
	const want = "order gate-sweep is not scheduled: its program $PACK_DIR/assets/scripts/gate-sweep.sh is a POSIX shell script"
	if got := stderr.String(); strings.Count(got, "is not scheduled") != 1 || !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want one line containing %q", got, want)
	}
}

// TestShellExecRunnerTimeoutReapsTheTreeOnWindows pins that an order command
// that outlives its deadline takes every process it started with it, because
// it runs in a Job Object that owns its tree.
func TestShellExecRunnerTimeoutReapsTheTreeOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := shellExecRunner(ctx, processgrouptest.DescendantCommand(record), t.TempDir(), nil); err == nil {
		t.Fatal("shellExecRunner returned without error; the command was supposed to outlive its deadline")
	}
	if d := processgrouptest.ReadDescendant(t, record); !d.WaitGone(10 * time.Second) {
		t.Fatal("the order command's descendant survived its cancellation")
	}
}

// TestShellExecRunnerWaitDelayReapsTheTreeOnWindows is the Windows twin of
// TestShellExecRunnerKillsProcessGroupAfterWaitDelay: an order command that
// exits while a descendant still holds its output is cleaned up once the wait
// delay runs out, and the cleanup reaps the descendant.
func TestShellExecRunnerWaitDelayReapsTheTreeOnWindows(t *testing.T) {
	oldWaitDelay := shellExecPostCancelWaitDelay
	shellExecPostCancelWaitDelay = 500 * time.Millisecond
	t.Cleanup(func() { shellExecPostCancelWaitDelay = oldWaitDelay })
	record := filepath.Join(t.TempDir(), "descendant")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := shellExecRunner(ctx, processgrouptest.LeaveDescendantCommand(record), t.TempDir(), nil); !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("shellExecRunner() error = %v, want %v", err, exec.ErrWaitDelay)
	}
	if d := processgrouptest.ReadDescendant(t, record); !d.WaitGone(10 * time.Second) {
		t.Fatal("the order command's descendant survived the cleanup after its wait delay")
	}
}

// TestFinishedOrderCommandLeavesItsDescendantRunningOnWindows pins the other
// half of the Unix contract: a process group outlives its leader, so an order
// command that finishes, holding nothing open, leaves what it started running.
// On Windows that is the difference between releasing the Job Object and
// killing it on release.
func TestFinishedOrderCommandLeavesItsDescendantRunningOnWindows(t *testing.T) {
	record := filepath.Join(t.TempDir(), "descendant")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := shellExecRunner(ctx, processgrouptest.LeaveDetachedDescendantCommand(record), t.TempDir(), nil); err != nil {
		t.Fatalf("shellExecRunner() error = %v, output %q; want the command to finish on its own", err, out)
	}
	if d := processgrouptest.ReadDescendant(t, record); d.WaitGone(2 * time.Second) {
		t.Fatal("the descendant died with the order command that finished; releasing its tree must not kill it")
	}
}
