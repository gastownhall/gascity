//go:build linux || darwin

package proctable

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
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

// TestKillLivenessFuncsAreIdentityBound pins the recycled-PID protection at the
// wiring, not just at the helper.
//
// The SIGTERM-grace probe used to be a bare kill(0) existence check, and it is
// what gates the SIGKILL wave: a target that answered SIGTERM and was reaped,
// whose PID was recycled before the grace expired, still read as "alive", so
// SIGKILL was sent to the new owner. signalPIDWith tries kill(-pid) first, so if
// that owner leads a process group — every tmux pane command and every Setpgid'd
// daemon does — the whole unrelated group dies and the call reports success.
//
// Both probes must therefore be bound to the ORIGINAL target's start-time
// identity: asked about a different live PID they must answer false. Restoring
// either to a bare existence check fails this.
func TestKillLivenessFuncsAreIdentityBound(t *testing.T) {
	self := os.Getpid()
	termLive, runLive := killLivenessFuncsForPID(self)

	if !termLive(self) {
		t.Error("termLive(self) = false; the probe must recognize its own target")
	}
	if !runLive(self) {
		t.Error("runLive(self) = false; the probe must recognize its own target")
	}

	// A live child with a start time distinct from ours stands in for a
	// recycled PID now owned by an unrelated process.
	recycled := startDistinctLiveProcess(t, self)
	if termLive(recycled) {
		t.Error("termLive reported a DIFFERENT live pid as our target: a recycled PID would receive a process-group SIGKILL")
	}
	if runLive(recycled) {
		t.Error("runLive reported a DIFFERENT live pid as our target")
	}
}

// startDistinctLiveProcess starts a long-lived child whose start-time identity
// differs from pid's, and returns the child's PID. Start times have clock-tick
// resolution on Linux (10ms) and one-second resolution through ps on darwin, so
// processes started close together share one. That is why PID 1 cannot stand
// in for "a different process": inside a fresh PID namespace (Bazel's
// linux-sandbox) init starts moments before the test binary and intermittently
// lands in the same tick, so the identity probe rightly answers "same
// process". A child that collides is discarded and another started.
func startDistinctLiveProcess(t *testing.T, pid int) int {
	t.Helper()
	own, err := pidutil.StartTime(pid)
	if err != nil || own == "" {
		t.Fatalf("start time of pid %d: %q, %v", pid, own, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			t.Fatalf("start stand-in process: %v", err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		started, err := pidutil.StartTime(cmd.Process.Pid)
		if err != nil || started == "" {
			t.Fatalf("start time of stand-in pid %d: %q, %v", cmd.Process.Pid, started, err)
		}
		if started != own {
			return cmd.Process.Pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("every stand-in process shared pid %d's start time %q", pid, own)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Kills: a kill not bound to the scanned process, or one that never kills.
// A changed start identity (a recycled PID: the scanned process is gone) and
// a gone PID are success without a signal; an unreadable identity is an error
// without a signal. Those signals are recorded, never sent. A matching
// identity kills a real child.
func TestKillByPIDIdentityRefusesRecycledPID(t *testing.T) {
	self := os.Getpid()
	for _, tc := range []struct {
		name    string
		read    func(int) (string, error)
		wantErr string
	}{
		{name: "recycled", read: func(int) (string, error) { return "999", nil }},
		{name: "unreadable", read: func(int) (string, error) { return "", errors.New("EIO") }, wantErr: "re-reading start identity"},
		{name: "gone", read: func(pid int) (string, error) { return "", fmt.Errorf("%w: PID %d", ErrProcessGone, pid) }},
	} {
		var signaled []string
		kill := func(pid int, sig syscall.Signal) error {
			signaled = append(signaled, fmt.Sprintf("%d:%v", pid, sig))
			return nil
		}
		err := killByPIDIdentity(self, "123", tc.read, kill)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: killByPIDIdentity = %v, want %q", tc.name, err, tc.wantErr)
		}
		if len(signaled) != 0 {
			t.Errorf("%s: signaled %v, want none", tc.name, signaled)
		}
	}

	t.Run("matching identity kills", func(t *testing.T) {
		child := startDistinctLiveProcess(t, self)
		identity, err := ProcessIdentity(child)
		if err != nil {
			t.Fatalf("ProcessIdentity(child): %v", err)
		}
		// KillByPIDIdentity returns nil only once the child is confirmed
		// dead: gone, or a zombie that can no longer run.
		if err := KillByPIDIdentity(child, identity); err != nil {
			t.Fatalf("KillByPIDIdentity(child, its identity) = %v, want nil", err)
		}
		if pidutil.Alive(child) {
			t.Fatalf("child %d still runnable after KillByPIDIdentity with its own identity", child)
		}
	})
}
