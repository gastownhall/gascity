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
	// ga-961qe1 / ga-cfr67u: this spawns a generic, un-setsid'd sleep 300
	// that is indistinguishable BY NAME from any other test's target —
	// including this file's own TestSetsidDoesNotPreventOrphanSelection,
	// which reuses this helper as a deliberate concurrent decoy. Logging the
	// pid here is what makes a given call's child distinguishable from any
	// other sleep 300 in test output/logs; callers must not rely on argv or
	// process name to tell two spawnReparentedChild children apart.
	t.Logf("spawnReparentedChild: spawned pid=%d (generic sleep 300, un-setsid'd)", pid)
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
//
// ga-961qe1 / ga-cfr67u: this test used to *discover* its own child after the
// fact via an unscoped, system-wide `pgrep -f 'sleep 300' | tail -1`, which
// has no way to tell this test's own target apart from any other `sleep 300`
// on a shared host. Two decoys prove that concretely rather than by
// inspection: an ambient one from spawnReparentedChild (this file's other
// helper, and exactly what ga-961qe1's root cause names as a real-world decoy
// source) started first, and a second, "race" one started immediately after
// the target. Linux allocates PIDs from a single monotonic counter, so a
// later fork reliably gets a higher PID within this short a window — the
// race decoy is guaranteed to be the highest-PID `sleep 300` match, and the
// OLD lookup, which always takes tail -1, is guaranteed to pick it instead of
// the real target. The race decoy and the pgrep lookup itself must run from
// within the *same* `sh -c` invocation that starts the target, not a
// separate exec.Command: pgrep -f matches full command lines, so a later,
// separate launcher's own `pgrep -f 'sleep 300'` argument text would itself
// contain the substring "sleep 300" and — being forked after everything else
// — would win tail -1 by matching *itself*, masking the real bug this test
// exists to show (this reproduction's own first draft hit exactly that).
// Identification of the target itself is captured directly via the combined
// script's own $! for the target (mirroring spawnReparentedChild's
// already-proven pattern), taken before the race decoy is started so it
// isn't overwritten, and is never used for the buggy lookup below — that
// lookup exists only to demonstrate the bug.
func TestSetsidDoesNotPreventOrphanSelection(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}
	sessionID := "ga-repro-s434i0-setsid-" + strconv.Itoa(os.Getpid())

	// Ambient decoy: some other, unrelated sleep 300 already on the host
	// before this test's own target even starts.
	ambientDecoyPID := spawnReparentedChild(t, os.Environ())

	// The setsid'd target, a same-shell race decoy started immediately after
	// it, and the OLD unscoped lookup — all from one `sh -c` invocation. See
	// the function comment for why the race decoy and the lookup cannot be
	// split into separate exec.Command calls without the lookup's own
	// launcher shell self-matching and winning tail -1.
	launcher := exec.Command("sh", "-c",
		"setsid sleep 300 >/dev/null 2>&1 & echo $!; "+
			"sleep 300 >/dev/null 2>&1 & echo $!; "+
			"sleep 0.2; "+
			"pgrep -f 'sleep 300' | tail -1")
	launcher.Env = append(os.Environ(), "GC_SESSION_ID="+sessionID)
	out, err := launcher.Output()
	if err != nil {
		t.Fatalf("spawn setsid child: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 3 {
		t.Fatalf("expected 3 pids (target, race decoy, identified) from launcher, got %q", out)
	}
	realPID, err := strconv.Atoi(lines[0])
	if err != nil {
		t.Fatalf("parse real target pid from %q: %v", out, err)
	}
	raceDecoyPID, err := strconv.Atoi(lines[1])
	if err != nil {
		t.Fatalf("parse race decoy pid from %q: %v", out, err)
	}
	pid, err := strconv.Atoi(lines[2])
	if err != nil {
		t.Fatalf("parse identified pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(realPID, syscall.SIGKILL) })
	t.Cleanup(func() { _ = syscall.Kill(raceDecoyPID, syscall.SIGKILL) })
	t.Logf("ambient decoy pid=%d, real target pid=%d, race decoy pid=%d, unscoped pgrep identified pid=%d",
		ambientDecoyPID, realPID, raceDecoyPID, pid)

	// OLD (buggy) identification picked tail -1 of an unscoped, system-wide
	// pgrep — confirm it landed on the race decoy specifically, and not the
	// real target, the ambient decoy, or some other stray match (e.g. the
	// lookup's own launcher shell — the exact confounder this reproduction's
	// own first draft hit; see the function comment).
	if pid != raceDecoyPID {
		t.Fatalf("reproduction setup did not race as intended: unscoped pgrep returned %d, "+
			"want the same-shell race decoy %d (real target is %d, ambient decoy is %d)",
			pid, raceDecoyPID, realPID, ambientDecoyPID)
	}

	// Prove the detachment is real: a fully setsid'd process leads its own
	// session and process group. This fires here — on the misidentified
	// decoy, not the real target — reproducing ga-961qe1's exact false
	// failure: the decoy is a normal child of its own launcher shell, not a
	// session leader, even though setsid worked fine for the real target.
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
