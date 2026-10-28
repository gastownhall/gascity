package pidutil

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// AliveWithStartTime closes the PID-reuse hole in Alive: during a post-SIGKILL
// reap wait the target's PID can be recycled to an unrelated process, at which
// point plain Alive wrongly reports the dead target as still alive.
//
// StartTime read only /proc/<pid>/stat, so off Linux it always errored, the
// identity check was skipped, and the hole stayed open. The visible consequence
// is the opposite of the reaper's: killByPID reports
// "PID %d still runnable %s after SIGKILL (not confirmed dead)" for a process
// that is genuinely dead, and internal/runtime/subprocess and the tmux adapter
// then refuse to start the replacement — an agent restart blocked by a
// protection that cannot function.

// TestStartTime_ReturnsValueOnThisHost is the regression test for the cause: a
// start-time identity must be obtainable on the host the code runs on.
func TestStartTime_ReturnsValueOnThisHost(t *testing.T) {
	got, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self) on %s: %v", runtime.GOOS, err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("StartTime(self) on %s returned an empty identity", runtime.GOOS)
	}
}

// TestAliveWithStartTime_RejectsMismatchedIdentity is the defect stated directly:
// a live PID whose recorded start time does not match must be reported dead,
// because that is what PID reuse looks like. Off Linux StartTime errored and the
// function returned true, leaving the reuse hole open.
func TestAliveWithStartTime_RejectsMismatchedIdentity(t *testing.T) {
	if got := AliveWithStartTime(os.Getpid(), "definitely-not-this-processes-start-time"); got {
		t.Fatalf("AliveWithStartTime(self, mismatched) = true on %s; a recycled PID would pass as the original process", runtime.GOOS)
	}
}

// TestAliveWithStartTime_AcceptsSameProcess is the over-correction guard: the
// real process must still be recognized. Passes before and after.
func TestAliveWithStartTime_AcceptsSameProcess(t *testing.T) {
	st, err := StartTime(os.Getpid())
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	if !AliveWithStartTime(os.Getpid(), st) {
		t.Fatalf("AliveWithStartTime(self, own start time %q) = false", st)
	}
}

// TestAliveWithStartTime_EmptyIdentityFallsBackToAlive pins the documented
// opt-out: no captured identity means no identity check.
func TestAliveWithStartTime_EmptyIdentityFallsBackToAlive(t *testing.T) {
	if !AliveWithStartTime(os.Getpid(), "") {
		t.Fatal("AliveWithStartTime(self, \"\") = false, want true (identity check disabled)")
	}
}
