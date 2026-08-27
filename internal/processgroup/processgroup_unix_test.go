//go:build !windows

package processgroup

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

func TestTerminateEscalatesToSIGKILL(t *testing.T) {
	killed := false
	var signals []syscall.Signal
	opts := Options{
		CurrentGroupID: func() int { return 12345 },
		PollPeriod:     time.Millisecond,
		Kill: func(_ int, sig syscall.Signal) error {
			switch sig {
			case syscall.SIGTERM, syscall.SIGKILL:
				signals = append(signals, sig)
				if sig == syscall.SIGKILL {
					killed = true
				}
				return nil
			case 0:
				if killed {
					return syscall.ESRCH
				}
				return nil
			default:
				t.Fatalf("unexpected signal %v", sig)
				return nil
			}
		},
	}

	if err := Terminate(45678, 0, opts); err != nil {
		t.Fatalf("Terminate() error = %v, want nil", err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals = %v, want [SIGTERM SIGKILL]", signals)
	}
}

func TestTerminateTreatsESRCHAsAlreadyStopped(t *testing.T) {
	opts := Options{
		CurrentGroupID: func() int { return 12345 },
		Kill: func(_ int, _ syscall.Signal) error {
			return syscall.ESRCH
		},
	}

	if err := Terminate(45678, time.Millisecond, opts); err != nil {
		t.Fatalf("Terminate() ESRCH error = %v, want nil", err)
	}
}

func TestTerminateRefusesCurrentProcessGroup(t *testing.T) {
	opts := Options{CurrentGroupID: func() int { return 45678 }}

	if err := Terminate(45678, time.Millisecond, opts); err == nil {
		t.Fatal("Terminate() current process group error = nil, want refusal")
	}
}

func TestTerminateCommandPreservesGroupFailureAfterDirectKill(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	err := TerminateCommand(cmd, syscall.Getpgrp(), time.Millisecond, Options{})
	if err == nil {
		t.Fatal("TerminateCommand() error = nil, want unsafe process group error")
	}
	if !strings.Contains(err.Error(), "refusing to signal unsafe process group") {
		t.Fatalf("TerminateCommand() error = %v, want unsafe process group detail", err)
	}
	_ = cmd.Wait()
}

// TestLeadsOwnGroupTrueForGroupLeader covers the case in which widening a
// signal from a process to a process group is sound: a child started via
// StartCommandInNewGroup leads the group its own pid names, so kill(-pid)
// reaches that child's tree and nothing else.
func TestLeadsOwnGroupTrueForGroupLeader(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := exec.Command("sleep", "30")
	StartCommandInNewGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start group leader: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if !LeadsOwnGroup(cmd.Process.Pid) {
		t.Fatalf("LeadsOwnGroup(%d) = false for a Setpgid child, want true", cmd.Process.Pid)
	}
}

// TestLeadsOwnGroupFalseForGroupMember is the case that makes this check
// load-bearing. A child started without Setpgid inherits its parent's group,
// so its pid is a group *member* id, not a group id. kill(-pid) would then
// name whatever unrelated group happens to hold that number — on a shared
// host, another agent's build (ga-8qmy).
func TestLeadsOwnGroupFalseForGroupMember(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start group member: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if LeadsOwnGroup(cmd.Process.Pid) {
		t.Fatalf("LeadsOwnGroup(%d) = true for a child that inherited its parent's group, want false", cmd.Process.Pid)
	}
}

// TestLeadsOwnGroupRefusesUnsafePIDs keeps the guard aligned with
// Terminate's: init and the nonpositive pids that give kill(2) its broadcast
// and current-group meanings are never group-signal targets.
func TestLeadsOwnGroupRefusesUnsafePIDs(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if LeadsOwnGroup(pid) {
			t.Errorf("LeadsOwnGroup(%d) = true, want false", pid)
		}
	}
}
