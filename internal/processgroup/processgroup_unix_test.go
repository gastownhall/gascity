//go:build !windows

package processgroup

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
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

// epermKill answers every signal with EPERM, the errno a Darwin kill(-pgid) gives
// both for a group whose members are all zombies and for a live member the
// caller may not signal. Only the member-state reader tells them apart.
func epermKill(_ int, _ syscall.Signal) error { return syscall.EPERM }

func liveMembers(n int, err error) func(int) (int, error) {
	return func(int) (int, error) { return n, err }
}

func epermOptions(live func(int) (int, error)) Options {
	return Options{
		CurrentGroupID: func() int { return 12345 },
		PollPeriod:     time.Millisecond,
		Kill:           epermKill,
		LiveMembers:    live,
	}
}

func startSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	processgrouptest.RequireRealProcessSignals(t)
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestEPERMFromAllZombieGroupIsGone(t *testing.T) {
	opts := epermOptions(liveMembers(0, nil))

	if err := Terminate(45678, time.Millisecond, opts); err != nil {
		t.Errorf("Terminate() = %v, want nil", err)
	}
	if err := InterruptCommand(&exec.Cmd{Process: &os.Process{Pid: 45678}}, 45678, opts); err != nil {
		t.Errorf("InterruptCommand() = %v, want nil", err)
	}
	if err := KillCommandGroup(&exec.Cmd{Process: &os.Process{Pid: 45678}}, 45678, opts); err != nil {
		t.Errorf("KillCommandGroup() = %v, want nil", err)
	}
	if alive(45678, opts) {
		t.Error("alive() = true, want false for a group of only zombies")
	}
}

func TestTerminateManagedCommandTreatsAllZombieEPERMAsGone(t *testing.T) {
	done := make(chan struct{})
	close(done)
	cmd := &exec.Cmd{Process: &os.Process{Pid: 45678}}

	err := terminateManagedCommandGraces(cmd, 45678, done, time.Second, time.Millisecond, epermOptions(liveMembers(0, nil)))
	if err != nil {
		t.Fatalf("terminateManagedCommandGraces() = %v, want nil", err)
	}
}

func TestEPERMWithLiveMemberKeepsTheError(t *testing.T) {
	opts := epermOptions(liveMembers(1, nil))

	if err := Terminate(45678, time.Millisecond, opts); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Terminate() = %v, want EPERM", err)
	}
	if err := InterruptCommand(&exec.Cmd{Process: &os.Process{Pid: 45678}}, 45678, opts); !errors.Is(err, syscall.EPERM) {
		t.Errorf("InterruptCommand() = %v, want EPERM", err)
	}
	if !alive(45678, opts) {
		t.Error("alive() = false, want true: a live member we may not signal is still there")
	}
	done := make(chan struct{})
	close(done)
	cmd := &exec.Cmd{Process: &os.Process{Pid: 45678}}
	if err := terminateManagedCommandGraces(cmd, 45678, done, time.Second, time.Millisecond, opts); !errors.Is(err, syscall.EPERM) {
		t.Errorf("terminateManagedCommandGraces() = %v, want EPERM", err)
	}
}

// KillCommandGroup falls back to a direct kill of the leader when the group
// signal fails, so the live-member case needs a real process to land that on.
func TestKillCommandGroupEPERMWithLiveMemberKeepsTheError(t *testing.T) {
	err := KillCommandGroup(startSleep(t), 45678, epermOptions(liveMembers(1, nil)))
	if !errors.Is(err, syscall.EPERM) {
		t.Errorf("KillCommandGroup() = %v, want EPERM", err)
	}
}

func TestEPERMWithUnreadableMembersKeepsTheError(t *testing.T) {
	opts := epermOptions(liveMembers(0, errors.New("sysctl failed")))

	if err := Terminate(45678, time.Millisecond, opts); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Terminate() = %v, want EPERM", err)
	}
	if !alive(45678, opts) {
		t.Error("alive() = false, want true when the group cannot be read")
	}
}

func TestEPERMIsUnchangedWithoutAMemberReader(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin reads the real process table")
	}
	opts := epermOptions(nil)

	if err := Terminate(45678, time.Millisecond, opts); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Terminate() = %v, want EPERM", err)
	}
	if !alive(45678, opts) {
		t.Error("alive() = false, want true: EPERM reads as alive off Darwin")
	}
}

// sigkillThenZombies models a group that outlives SIGTERM and the grace period
// and then has no member left to signal by the time SIGKILL is sent: every
// signal before SIGKILL succeeds (a live member answers signal 0), and every
// signal from SIGKILL on answers EPERM, the errno a Darwin kill(-pgid) gives a
// group of only zombies.
func sigkillThenZombies() func(int, syscall.Signal) error {
	sigkilled := false
	return func(_ int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			sigkilled = true
		}
		if sigkilled {
			return syscall.EPERM
		}
		return nil
	}
}

// Terminate reaches SIGKILL only when SIGTERM did not end the group within the
// grace period, so an EPERM at that second site needs a pin of its own.
func TestTerminateSIGKILLEPERMFromAllZombieGroupIsGone(t *testing.T) {
	opts := epermOptions(liveMembers(0, nil))
	opts.Kill = sigkillThenZombies()

	if err := Terminate(45678, 5*time.Millisecond, opts); err != nil {
		t.Errorf("Terminate() = %v, want nil: the group holds only zombies once SIGKILL answers EPERM", err)
	}
}

func TestTerminateSIGKILLEPERMWithLiveMemberKeepsTheError(t *testing.T) {
	opts := epermOptions(liveMembers(1, nil))
	opts.Kill = sigkillThenZombies()

	if err := Terminate(45678, 5*time.Millisecond, opts); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Terminate() = %v, want EPERM: a live member we may not signal is still there", err)
	}
}
