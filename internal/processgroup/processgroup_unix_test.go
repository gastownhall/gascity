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
		EPERMSettle:    5 * time.Millisecond,
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

// exitingMember models the window Darwin opens when a group's only member has
// begun to exit on its own (an agent that reads EOF on stdin as Stop closes it):
// the member is not yet a zombie, so the member reader counts it live, and
// kill(-pgid, sig) answers EPERM until the exit settles. After settle signals
// the group is cleared: it takes the signal (end nil) or has nothing left to
// signal (end ESRCH).
type exitingMember struct {
	settle int
	end    error
	calls  int
	sent   bool
}

func (m *exitingMember) kill(_ int, sig syscall.Signal) error {
	if sig == 0 {
		if m.sent {
			return syscall.ESRCH
		}
		return syscall.EPERM
	}
	m.calls++
	if m.calls <= m.settle {
		return syscall.EPERM
	}
	m.sent = true
	return m.end
}

func exitingOptions(m *exitingMember) Options {
	opts := epermOptions(liveMembers(1, nil))
	opts.Kill = m.kill
	opts.EPERMSettle = 2 * time.Second
	return opts
}

func TestEPERMFromExitingMemberSettlesAtEverySignalSite(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: 45678}}
	for _, end := range []error{nil, syscall.ESRCH} {
		sites := map[string]func(Options) error{
			"Terminate": func(o Options) error { return Terminate(45678, time.Second, o) },
			"InterruptCommand": func(o Options) error {
				return InterruptCommand(cmd, 45678, o)
			},
			"KillCommandGroup": func(o Options) error {
				return KillCommandGroup(cmd, 45678, o)
			},
			"terminateManagedCommandGraces": func(o Options) error {
				done := make(chan struct{})
				close(done)
				return terminateManagedCommandGraces(cmd, 45678, done, time.Second, time.Millisecond, o)
			},
		}
		for name, site := range sites {
			m := &exitingMember{settle: 3, end: end}
			if err := site(exitingOptions(m)); err != nil {
				t.Errorf("%s (member exit ends %v) = %v, want nil: EPERM from a member that is exiting settles", name, end, err)
			}
			if m.calls <= m.settle {
				t.Errorf("%s (member exit ends %v) sent %d signals, want more than %d: the EPERM was not retried", name, end, m.calls, m.settle)
			}
		}
	}
}

func TestEPERMFromExitingMemberThatBecomesZombieIsGone(t *testing.T) {
	reads := 0
	opts := epermOptions(func(int) (int, error) {
		reads++
		if reads <= 2 {
			return 1, nil
		}
		return 0, nil
	})
	opts.EPERMSettle = 2 * time.Second

	if err := Terminate(45678, time.Second, opts); err != nil {
		t.Errorf("Terminate() = %v, want nil: the member became a zombie while the EPERM was settling", err)
	}
	if reads <= 2 {
		t.Errorf("member reader consulted %d times, want more than 2: the EPERM was not retried", reads)
	}
}

func TestPersistentEPERMErrorCarriesMemberOutcome(t *testing.T) {
	cmd := &exec.Cmd{Process: &os.Process{Pid: 45678}}
	states := func(int) ([]MemberState, error) {
		return []MemberState{{Pid: 4242, State: "SRUN"}, {Pid: 4243, State: "SZOMB"}}, nil
	}
	cases := map[string]struct {
		live func(int) (int, error)
		want []string
	}{
		"live member": {liveMembers(1, nil), []string{"EPERM", "live members=1", "pid 4242 SRUN", "pid 4243 SZOMB"}},
		"unreadable":  {liveMembers(0, errors.New("sysctl failed")), []string{"EPERM", "sysctl failed", "pid 4242 SRUN"}},
	}
	for name, c := range cases {
		opts := epermOptions(c.live)
		opts.MemberStates = states
		sites := map[string]func() error{
			"Terminate":        func() error { return Terminate(45678, time.Millisecond, opts) },
			"InterruptCommand": func() error { return InterruptCommand(cmd, 45678, opts) },
			"terminateManagedCommandGraces": func() error {
				done := make(chan struct{})
				close(done)
				return terminateManagedCommandGraces(cmd, 45678, done, time.Second, time.Millisecond, opts)
			},
		}
		for site, run := range sites {
			err := run()
			if !errors.Is(err, syscall.EPERM) {
				t.Errorf("%s/%s = %v, want EPERM kept", name, site, err)
				continue
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s/%s error = %q, want it to contain %q", name, site, err, want)
				}
			}
		}
	}
}

func TestPersistentEPERMErrorSaysWhenMemberStatesCannotBeRead(t *testing.T) {
	opts := epermOptions(liveMembers(1, nil))
	opts.MemberStates = func(int) ([]MemberState, error) { return nil, errors.New("kinfo unavailable") }

	err := Terminate(45678, time.Millisecond, opts)
	if !errors.Is(err, syscall.EPERM) || !strings.Contains(err.Error(), "kinfo unavailable") {
		t.Errorf("Terminate() = %v, want EPERM naming the state-read failure", err)
	}
}

// Off Darwin there is no member reader, and a Darwin read that fails reads the
// same: an EPERM there cannot be told apart by waiting, so it is reported on the
// first signal rather than after the settle window.
func TestEPERMWithUnreadableMembersIsNotRetried(t *testing.T) {
	calls := 0
	opts := epermOptions(liveMembers(0, errors.New("sysctl failed")))
	opts.EPERMSettle = time.Hour
	opts.Kill = func(int, syscall.Signal) error {
		calls++
		return syscall.EPERM
	}

	err := InterruptCommand(&exec.Cmd{Process: &os.Process{Pid: 45678}}, 45678, opts)
	if !errors.Is(err, syscall.EPERM) {
		t.Errorf("InterruptCommand() = %v, want EPERM", err)
	}
	if err != nil && (!strings.Contains(err.Error(), "not retried") || strings.Contains(err.Error(), "after ")) {
		t.Errorf("InterruptCommand() error = %q, want it to say the signal was not retried, with no settle window", err)
	}
	if calls != 1 {
		t.Errorf("signal sent %d times, want 1: an unreadable group is not retried", calls)
	}
}
