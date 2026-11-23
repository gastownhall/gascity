//go:build windows

package pidutil

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// startLongLivedChild starts a child that outlives the test and returns its
// PID. The child is killed on cleanup.
func startLongLivedChild(t *testing.T) int {
	t.Helper()
	cmd, _ := longLivedTestCommand()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting long-lived child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// TestStartTimeTokenDistinguishesTwoLiveProcesses is the PID-reuse defence
// stated as the property that makes it work: the token must be a property of
// the process, not of the PID.
//
// A recycled PID cannot be staged on demand, so this proves the discriminating
// half directly — two processes must carry different tokens, and each must be
// rejected when checked against the other's. If tokens collided, a recycled PID
// would pass as its predecessor and a reaper would report a dead process as
// still running, or a supervisor would adopt a stranger as its own child.
func TestStartTimeTokenDistinguishesTwoLiveProcesses(t *testing.T) {
	firstPID := startLongLivedChild(t)
	// Windows creation time has 100ns resolution, but processes started in the
	// same tick would share a token. Separate them beyond one tick.
	time.Sleep(20 * time.Millisecond)
	secondPID := startLongLivedChild(t)

	firstToken, err := StartTime(firstPID)
	if err != nil {
		t.Fatalf("StartTime(%d): %v", firstPID, err)
	}
	secondToken, err := StartTime(secondPID)
	if err != nil {
		t.Fatalf("StartTime(%d): %v", secondPID, err)
	}
	if firstToken == "" || secondToken == "" {
		t.Fatalf("StartTime returned an empty token: first=%q second=%q", firstToken, secondToken)
	}
	if firstToken == secondToken {
		t.Fatalf("two distinct processes share the identity token %q; a recycled PID would pass as its predecessor", firstToken)
	}

	if !AliveWithStartTime(firstPID, firstToken) {
		t.Fatalf("AliveWithStartTime(%d, own token) = false for a live process", firstPID)
	}
	if !AliveWithStartTime(secondPID, secondToken) {
		t.Fatalf("AliveWithStartTime(%d, own token) = false for a live process", secondPID)
	}
	// The cross-check is the recycled-PID case in miniature: right PID number,
	// wrong process.
	if AliveWithStartTime(firstPID, secondToken) {
		t.Fatalf("AliveWithStartTime(%d, another process's token) = true; the token does not actually identify the process", firstPID)
	}
}

// TestStartTimeTokenIsStableAcrossReads pins the other half: a token that
// drifted between reads would reject the very process it identifies, which
// presents as a live process reported dead.
func TestStartTimeTokenIsStableAcrossReads(t *testing.T) {
	pid := startLongLivedChild(t)
	first, err := StartTime(pid)
	if err != nil {
		t.Fatalf("StartTime(%d): %v", pid, err)
	}
	time.Sleep(50 * time.Millisecond)
	second, err := StartTime(pid)
	if err != nil {
		t.Fatalf("StartTime(%d) second read: %v", pid, err)
	}
	if first != second {
		t.Fatalf("identity token changed across reads for a single live process: %q then %q", first, second)
	}
}

// TestAliveReportsProcessExitedWith259AsDead is the STILL_ACTIVE collision.
//
// GetExitCodeProcess reports 259 both for "still running" and for a process
// that genuinely exited with code 259, so an exit-code-based liveness check
// cannot tell them apart and reports the dead process as alive forever. A
// reaper waiting for confirmed death then never returns.
//
// A handle is opened before the child exits and held for the duration: while
// any handle is open the kernel keeps the process object, so the PID still
// resolves and the liveness check has something to get wrong.
func TestAliveReportsProcessExitedWith259AsDead(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit 259")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	pid := cmd.Process.Pid

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("pinning process object for pid %d: %v", pid, err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	_ = cmd.Wait()

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		t.Fatalf("GetExitCodeProcess(%d): %v", pid, err)
	}
	if code != 259 {
		t.Skipf("child exited with %d, not 259; the STILL_ACTIVE collision cannot be staged here", code)
	}

	if Alive(pid) {
		t.Fatalf("Alive(%d) = true for a process that exited with code 259; exit code 259 is indistinguishable from STILL_ACTIVE and must not be read as liveness", pid)
	}
	if AliveWithStartTime(pid, "any-captured-token") {
		t.Fatalf("AliveWithStartTime(%d) = true for a process that exited with code 259", pid)
	}
}

// TestAliveReportsAccessDeniedProcessAsAlive pins the direction an observation
// failure must fail in. A process this token cannot open still exists, and
// Unix says so by treating EPERM as alive; reading access-denied as death
// would let a caller start a second copy of a singleton alongside the first,
// or report a cleanup complete while the process runs on.
func TestAliveReportsAccessDeniedProcessAsAlive(t *testing.T) {
	pid := accessDeniedPID(t)
	if !Alive(pid) {
		t.Fatalf("Alive(%d) = false for a live process this token may not open; access denied is not death", pid)
	}
}

// accessDeniedPID finds a live PID that cannot be opened for query because of
// access rights, skipping when the environment cannot provide one.
func accessDeniedPID(t *testing.T) int {
	t.Helper()
	// PID 4 is the System process, which an unprivileged token cannot open.
	// Running elevated may be able to, in which case the case is unavailable.
	const systemPID = 4
	_, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, systemPID)
	if err == nil {
		t.Skip("this token can open the System process, so the access-denied case cannot be staged; run unelevated to exercise it")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("opening the System process failed with %v rather than access denied; the case cannot be staged here", err)
	}
	return systemPID
}

// TestAliveWithCmdlineOnDeadProcessIsACleanNegative pins the boundary of the
// rule above: once the process is genuinely gone, the answer is a plain
// negative with no error, because nothing needs identifying.
func TestAliveWithCmdlineOnDeadProcessIsACleanNegative(t *testing.T) {
	cmd := shortLivedTestCommand()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		matched, err := AliveWithCmdline(pid, func([]string) bool { return true })
		if !matched && err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("AliveWithCmdline(%d) never settled to a clean negative for an exited process", pid)
}
