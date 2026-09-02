//go:build linux

package proctable

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// spawnReparentedChild starts a long-lived `sleep` from a short-lived shell so
// the sleep outlives its immediate parent and is reparented to init (or the
// user-manager subreaper). That is exactly the shape `gc supervisor start`
// leaves behind: doSupervisorStartJSON forks `gc supervisor run` and then
// returns, abandoning the child. env is handed to the child verbatim, mirroring
// the `child.Env = os.Environ()` in cmd/gc/cmd_supervisor_lifecycle.go.
func spawnReparentedChild(t *testing.T, env []string) int {
	t.Helper()
	launcher := exec.Command("sh", "-c", "sleep 300 >/dev/null 2>&1 & echo $!")
	launcher.Env = env
	out, err := launcher.Output()
	if err != nil {
		t.Fatalf("spawn launcher: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse child pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func scanFindsPID(t *testing.T, sessionID string, pid int) bool {
	t.Helper()
	found, err := scanWithRoot("/proc", sessionID)
	if err != nil {
		// Permission noise from other users' processes is expected and
		// non-fatal; the scanner joins those errors and still returns matches.
		t.Logf("scanWithRoot returned non-fatal errors: %v", err)
	}
	for _, r := range found {
		if r.PID == pid {
			return true
		}
	}
	return false
}

// TestBareForkedSupervisorIsSelectedAsOrphanKillTarget reproduces ga-s434i0.
//
// A supervisor forked by `gc supervisor start` from inside an agent's tmux pane
// inherits that pane's GC_SESSION_ID. Once its launcher exits, the supervisor is
// reparented, which makes isRootWithSessionID classify it as an *agent root*.
// Manager.killExistingOrphans then selects it at the next pre-start of that same
// session bead and proctable.KillByPID SIGTERMs it.
//
// The assertion is the bug: a process that is not an agent runtime at all is
// returned as a terminate-me runtime purely because it inherited the env.
func TestBareForkedSupervisorIsSelectedAsOrphanKillTarget(t *testing.T) {
	sessionID := "ga-repro-s434i0-" + strconv.Itoa(os.Getpid())
	cityPath := t.TempDir()

	paneEnv := append(os.Environ(),
		"GC_SESSION_ID="+sessionID,
		"GC_CITY_PATH="+cityPath,
	)
	pid := spawnReparentedChild(t, paneEnv)

	if !scanFindsPID(t, sessionID, pid) {
		t.Fatalf("pid %d carrying GC_SESSION_ID=%s was NOT returned by the orphan scan; "+
			"the ga-s434i0 mechanism did not reproduce", pid, sessionID)
	}
	t.Logf("REPRODUCED: pid %d (a non-agent process) is selected as an orphan "+
		"kill target for session %s solely because it inherited GC_SESSION_ID", pid, sessionID)
}

// TestScrubbedForkedSupervisorIsNotSelected is the fix oracle: with the
// launching session's identity stripped from the child's environment, the same
// reparented process is invisible to the orphan sweep.
func TestScrubbedForkedSupervisorIsNotSelected(t *testing.T) {
	sessionID := "ga-repro-s434i0-scrubbed-" + strconv.Itoa(os.Getpid())

	var scrubbed []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GC_SESSION_ID=") {
			continue
		}
		scrubbed = append(scrubbed, kv)
	}
	pid := spawnReparentedChild(t, scrubbed)

	if scanFindsPID(t, sessionID, pid) {
		t.Fatalf("pid %d was selected despite a scrubbed GC_SESSION_ID", pid)
	}
	t.Logf("FIX ORACLE: pid %d with GC_SESSION_ID scrubbed is not an orphan kill target", pid)
}

// TestSetsidDoesNotPreventOrphanSelection refutes the ga-s434i0 filing's
// proposed remedy #3 ("the fork path should Setsid so recycling the launching
// session cannot signal it").
//
// Setsid detaches the child from the launcher's session and process group, so
// it defeats signals aimed at a *group* or delivered via a closing controlling
// terminal. It does nothing here, because this kill path never targets a group
// it inferred from the pane:
//
//   - selection reads /proc/<pid>/environ for GC_SESSION_ID and classifies
//     rootness from PPID (scan_linux.go:83, :160-176). Neither is affected by
//     setsid.
//   - delivery is proctable.signalPIDWith (kill_unix.go), which tries
//     kill(-pid, SIGTERM) and *falls back to kill(pid, SIGTERM)*. A private
//     session/group does not make the process unaddressable by its own PID.
//
// The child below is setsid'd — a strictly stronger detachment than the
// Setpgid the real fork path uses — and is still selected.
func TestSetsidDoesNotPreventOrphanSelection(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	sessionID := "ga-repro-s434i0-setsid-" + strconv.Itoa(os.Getpid())

	launcher := exec.Command("sh", "-c", "setsid sleep 300 >/dev/null 2>&1 & sleep 0.2; pgrep -f 'sleep 300' | tail -1")
	launcher.Env = append(os.Environ(), "GC_SESSION_ID="+sessionID)
	out, err := launcher.Output()
	if err != nil {
		t.Fatalf("spawn setsid child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse setsid child pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	// Prove the detachment is real: a fully setsid'd process leads its own
	// session and process group.
	sid := procStatField(t, pid, 3)
	if sid != pid {
		t.Fatalf("child %d is not a session leader (sid=%d); setsid did not take effect", pid, sid)
	}

	if !scanFindsPID(t, sessionID, pid) {
		t.Fatalf("setsid'd pid %d was NOT selected — refutation failed", pid)
	}
	t.Logf("REFUTED remedy #3: pid %d leads its own session (sid=%d) and is STILL "+
		"selected as an orphan kill target; setsid does not close this hole", pid, sid)
}

// procStatField returns the n-th whitespace-separated field of /proc/<pid>/stat
// counted from the field immediately after comm (n=0 state, 1 ppid, 2 pgrp,
// 3 session), parsed as an int.
func procStatField(t *testing.T, pid, n int) int {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatalf("read stat for %d: %v", pid, err)
	}
	text := string(data)
	closeParen := strings.LastIndex(text, ")")
	if closeParen < 0 {
		t.Fatalf("malformed stat for %d", pid)
	}
	fields := strings.Fields(text[closeParen+1:])
	if len(fields) <= n {
		t.Fatalf("stat for %d has %d fields, want > %d", pid, len(fields), n)
	}
	v, err := strconv.Atoi(fields[n])
	if err != nil {
		t.Fatalf("parse stat field %d for %d: %v", n, pid, err)
	}
	return v
}
