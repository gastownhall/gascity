//go:build !windows

package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

func TestTerminateManagedCommandBoundsReap(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nilProcess  bool
		exitOnKill  bool
		wantTimeout bool
	}{
		{name: "stuck group", wantTimeout: true},
		{name: "stuck without process", nilProcess: true, wantTimeout: true},
		{name: "reaped after kill", exitOnKill: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan struct{})
			cmd := &exec.Cmd{}
			if !tc.nilProcess {
				cmd.Process = &os.Process{Pid: 45678}
			}
			var signals []syscall.Signal
			opts := Options{
				CurrentGroupID: func() int { return 12345 },
				Kill: func(_ int, sig syscall.Signal) error {
					signals = append(signals, sig)
					if sig == syscall.SIGKILL && tc.exitOnKill {
						close(done)
					}
					return nil
				},
			}
			err := terminateManagedCommandGraces(cmd, 45678, done, 0, time.Millisecond, opts)
			if got := errors.Is(err, ErrProcessNotConfirmedDead); got != tc.wantTimeout {
				t.Fatalf("terminate error = %v, want timeout %v", err, tc.wantTimeout)
			}
			if !tc.wantTimeout && err != nil {
				t.Fatalf("terminate error = %v, want nil", err)
			}
			if !tc.nilProcess && (len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL) {
				t.Fatalf("signals = %v, want [SIGTERM SIGKILL]", signals)
			}
		})
	}
}

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
