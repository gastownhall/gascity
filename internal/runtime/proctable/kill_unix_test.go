//go:build linux || darwin

package proctable

import (
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
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
	var got []int
	err := signalPIDWith(12345, syscall.SIGTERM, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", sig)
		}
		got = append(got, pid)
		if pid < 0 {
			return syscall.ESRCH
		}
		return nil
	})
	if err != nil {
		t.Fatalf("signalPIDWith(): %v", err)
	}
	want := []int{-12345, 12345}
	if len(got) != len(want) {
		t.Fatalf("signal calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("signal calls = %v, want %v", got, want)
		}
	}
}

func TestSignalPIDGroupSuccessSkipsFallback(t *testing.T) {
	var got []int
	err := signalPIDWith(12345, syscall.SIGTERM, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", sig)
		}
		got = append(got, pid)
		return nil
	})
	if err != nil {
		t.Fatalf("signalPIDWith(): %v", err)
	}
	want := []int{-12345}
	if !slices.Equal(got, want) {
		t.Fatalf("signal calls = %v, want %v", got, want)
	}
}

// TestKillByPIDConfirmedDeadBeforeReturn drives the injected core: a process
// still runnable after SIGKILL (e.g. wedged in D-state) must yield an error so
// a caller can refuse to start a racing replacement, while one that becomes
// dead (gone or zombie) after SIGKILL returns nil.
func TestKillByPIDConfirmedDeadBeforeReturn(t *testing.T) {
	t.Run("survives SIGKILL -> error", func(t *testing.T) {
		var signals []syscall.Signal
		kill := func(_ int, sig syscall.Signal) error {
			// Record every delivery attempt. signalPIDWith signals the process
			// group (negative pid) first and returns on success, so with this
			// always-succeeding fake these are the group deliveries; the
			// assertion below only checks the final escalation is SIGKILL.
			signals = append(signals, sig)
			return nil
		}
		termLive := func(int) bool { return true } // never exits on SIGTERM
		runLive := func(int) bool { return true }  // survives SIGKILL too
		err := killByPID(4321, kill, termLive, runLive, 5*time.Millisecond, 5*time.Millisecond)
		if err == nil {
			t.Fatal("killByPID returned nil for a process that survived SIGKILL")
		}
		if !strings.Contains(err.Error(), "not confirmed dead") {
			t.Fatalf("error = %v, want 'not confirmed dead'", err)
		}
		if len(signals) == 0 || signals[len(signals)-1] != syscall.SIGKILL {
			t.Fatalf("signals = %v, want SIGKILL escalation", signals)
		}
	})

	t.Run("dies after SIGKILL -> nil", func(t *testing.T) {
		kill := func(int, syscall.Signal) error { return nil }
		termLive := func(int) bool { return true } // ignores SIGTERM
		var kills int
		runLive := func(int) bool {
			kills++
			return kills <= 1 // alive on first confirm poll, dead after
		}
		if err := killByPID(4321, kill, termLive, runLive, 5*time.Millisecond, time.Second); err != nil {
			t.Fatalf("killByPID: %v", err)
		}
	})

	t.Run("exits during SIGTERM grace -> no SIGKILL", func(t *testing.T) {
		var sawKill bool
		kill := func(_ int, sig syscall.Signal) error {
			if sig == syscall.SIGKILL {
				sawKill = true
			}
			return nil
		}
		var polls int
		termLive := func(int) bool {
			polls++
			return polls <= 1 // alive at entry, exits before grace elapses
		}
		runLive := func(int) bool { return false }
		if err := killByPID(4321, kill, termLive, runLive, time.Second, time.Second); err != nil {
			t.Fatalf("killByPID: %v", err)
		}
		if sawKill {
			t.Fatal("SIGKILL sent even though the process exited during grace")
		}
	})
}

func TestWaitUntilRespectsZeroTimeout(t *testing.T) {
	if !waitUntil(func() bool { return true }, 0) {
		t.Fatal("waitUntil should observe an already-satisfied condition at zero timeout")
	}
	if waitUntil(func() bool { return false }, 0) {
		t.Fatal("waitUntil should report false when the condition never holds at zero timeout")
	}
}

// stubKillByPIDDeps swaps KillByPID's injected dependencies for one test and
// restores them on cleanup, so the infrastructure refusal is exercised through
// KillByPID itself with no process spawned and nothing read from the host. A
// guard that is correct but unreferenced is exactly the failure mode the first
// fix for gastownhall/gascity#5837 had: installed on a different kill family
// than the one that fired.
func stubKillByPIDDeps(t *testing.T, infra func(int) bool, alive func(int) bool, kill func(int, syscall.Signal) error) {
	t.Helper()
	prevInfra, prevAlive, prevKill, prevStart := killByPIDInfrastructureTarget, killByPIDAlive, killByPIDSignal, killByPIDStartTime
	killByPIDInfrastructureTarget, killByPIDAlive, killByPIDSignal = infra, alive, kill
	killByPIDStartTime = func(int) (string, error) { return "", nil }
	t.Cleanup(func() {
		killByPIDInfrastructureTarget, killByPIDAlive, killByPIDSignal, killByPIDStartTime = prevInfra, prevAlive, prevKill, prevStart
	})
}

// TestKillByPIDRefusesTmuxInfrastructureTarget fails immediately without the
// guard: the liveness stub says alive, so KillByPID would signal the group,
// and the signal stub fails the test on the first delivery.
func TestKillByPIDRefusesTmuxInfrastructureTarget(t *testing.T) {
	stubKillByPIDDeps(t,
		func(pid int) bool { return pid == 4242 },
		func(int) bool { return true },
		func(pid int, sig syscall.Signal) error {
			t.Fatalf("KillByPID signaled %d with %v: the tmux-infrastructure guard is not wired in", pid, sig)
			return nil
		},
	)
	if err := KillByPID(4242); err != nil {
		t.Fatalf("KillByPID(tmux infrastructure) = %v, want nil: refusing is success", err)
	}
}

// TestKillByPIDStillSignalsNonInfrastructureTarget pins that the guard does
// not over-refuse: an agent PID still gets the group SIGTERM. The liveness
// stub reports alive at entry and gone on the next poll, so neither grace
// window is waited on; the SIGKILL escalation stays covered by the killByPID
// core tests above.
func TestKillByPIDStillSignalsNonInfrastructureTarget(t *testing.T) {
	var signals []int
	polls := 0
	stubKillByPIDDeps(t,
		func(int) bool { return false },
		func(int) bool { polls++; return polls <= 1 },
		func(pid int, sig syscall.Signal) error {
			if sig != syscall.SIGTERM {
				t.Fatalf("signal = %v, want SIGTERM", sig)
			}
			signals = append(signals, pid)
			return nil
		},
	)
	if err := KillByPID(4242); err != nil {
		t.Fatalf("KillByPID(agent) = %v, want nil", err)
	}
	if !slices.Equal(signals, []int{-4242}) {
		t.Fatalf("signals = %v, want the process group signaled once", signals)
	}
}
