//go:build !windows

package execgrace

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestRootCause_LeaderOnlySignalMissesForegroundChild deterministically forces
// the exact mechanism behind the foreground-child fork race: the interrupt is
// delivered to the process group at the precise instant a foreground child
// (mimicking a readiness delay) has not yet been fork()'d into that group —
// so only the shell leader receives it. A POSIX shell defers a trap while it
// is waiting on a foreground child, so once that child does appear moments
// later, the pending trap cannot run until the child exits naturally, and a
// grace-period force-kill kills the whole tree first: rollback never runs.
//
// This test reproduces that outcome with certainty by signaling the shell
// leader PID directly (bypassing the process group entirely), which is
// behaviorally identical to "the child was not yet a group member" from the
// shell's point of view — no child receives the signal either way.
func TestRootCause_LeaderOnlySignalMissesForegroundChild(t *testing.T) {
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	interruptFile := filepath.Join(dir, "interrupted")
	script := fmt.Sprintf(`
trap 'printf "%%s\n" interrupted > "%s"; exit 0' INT
: > "%s"
sleep 30
`, interruptFile, readyFile)

	cmd := exec.Command("sh", "-c", script)
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	waitForFile(t, readyFile)

	// Leader-only delivery: excludes whatever foreground child exists (or is
	// about to exist) from the signal, exactly as a fork-race miss would.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal leader: %v", err)
	}

	// The trap must NOT run promptly: it is deferred behind the foreground
	// `sleep 30`, which never received a signal and will run to completion.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(interruptFile); err == nil {
		t.Fatal("rollback trap ran despite leader-only signal — root-cause hypothesis not confirmed")
	}
	t.Log("confirmed: leader-only signal leaves the rollback trap deferred behind the foreground child")
}

// TestInterruptProcessGroupRetryReachesLateJoiningMember proves the fix
// end-to-end: interruptProcessGroup must retry the process-group SIGINT so a
// process that joins the group after the first delivery still receives it.
//
// A live fork() race (a real adapter forking its foreground child at the
// exact instant the first SIGINT is sent) is inherently timing-dependent —
// that unpredictability is *why* the underlying bug was only a 1-in-20 flake
// upstream, and is also awkward to stage reliably with a shell script here:
// a shell that ignores SIGINT so it can survive an early delivery also makes
// every process it forks afterward ignore SIGINT too, since POSIX shells
// refuse to let a script re-trap a signal that was already ignored on entry
// (that inherited-ignore behavior is exactly what
// TestRootCause_LeaderOnlySignalMissesForegroundChild above relies on for its
// *own* determinism, which is why it can't double as this test too). So
// instead of racing a real fork, this test controls "late group membership"
// directly and deterministically: a plain Go helper process (this same test
// binary, re-invoked) starts outside the target group, waits a fixed delay,
// and only then calls setpgid(0, ...) on ITSELF to join it — the exact
// condition interruptProcessGroup's retry loop must handle, without any
// shell trap-deferral semantics muddying the result. (The parent can't do
// the setpgid from the outside once the child has exec'd — POSIX only allows
// that within a narrow pre-exec window — so the child joins itself.)
//
// Unpatched (single kill(2), no retry) this test fails: interruptProcessGroup
// returns immediately after one kill(-pgid) sent before the late helper
// joins, so the helper is never signaled and the test times out waiting for
// its marker file. Patched, the 300ms/10ms retry budget keeps resending to
// the group and one of those resends lands after the late helper has joined.
func TestInterruptProcessGroupRetryReachesLateJoiningMember(t *testing.T) {
	dir := t.TempDir()
	interruptFile := filepath.Join(dir, "interrupted")

	// Leader ignores SIGINT forever (rather than dying on the first
	// delivery) purely so its process group stays alive — with a fixed
	// numeric pgid the retry loop keeps addressing — for the whole test,
	// regardless of how many times the group gets signaled. It writes
	// readyFile only once its SIGINT handler is actually registered, so the
	// test never has to guess how long Go binary startup takes before
	// sending the first signal (a real race, but not the one under test).
	readyFile := filepath.Join(dir, "leader-ready")
	leader := exec.Command(os.Args[0], "-test.run=TestSignalUnixHelperProcess")
	leader.Env = append(os.Environ(), "HELPER_ROLE=leader", "HELPER_READY_FILE="+readyFile)
	setProcessGroup(leader)
	if err := leader.Start(); err != nil {
		t.Fatalf("start leader: %v", err)
	}
	t.Cleanup(func() { _ = leader.Process.Kill(); _, _ = leader.Process.Wait() })

	waitForFile(t, readyFile)

	leaderPgid, err := syscall.Getpgid(leader.Process.Pid)
	if err != nil {
		t.Fatalf("getpgid(leader): %v", err)
	}

	// Starts in its own default group. After a fixed 50ms delay it joins
	// leaderPgid itself (self-setpgid, allowed post-exec where an external
	// setpgid from this test process would not be), simulating a process
	// that becomes a group member only after interruptProcessGroup's first
	// attempt has already gone out.
	late := exec.Command(os.Args[0], "-test.run=TestSignalUnixHelperProcess")
	late.Env = append(os.Environ(),
		"HELPER_ROLE=late",
		"HELPER_MARKER_FILE="+interruptFile,
		"HELPER_JOIN_PGID="+strconv.Itoa(leaderPgid),
		"HELPER_JOIN_DELAY_MS=50",
	)
	if err := late.Start(); err != nil {
		t.Fatalf("start late member: %v", err)
	}
	t.Cleanup(func() { _ = late.Process.Kill(); _, _ = late.Process.Wait() })

	// Fires now, while `late` is still outside leaderPgid — this is the "not
	// yet a group member" condition. It must miss on this attempt no matter
	// what; only a later retry (after late's 50ms self-join) can reach it.
	if err := interruptProcessGroup(leader); err != nil {
		t.Fatalf("interruptProcessGroup: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(interruptFile); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("rollback marker never appeared — retry did not reach the late-joining group member")
}

// TestSignalUnixHelperProcess is not a real test: it is the well-known Go
// os/exec "helper process" idiom (see the standard library's own
// os/exec_test.go), re-invoking this test binary as a plain subprocess. It
// is a no-op unless HELPER_ROLE is set, so it does nothing under a normal
// `go test` run.
func TestSignalUnixHelperProcess(_ *testing.T) {
	switch os.Getenv("HELPER_ROLE") {
	case "leader":
		// Ignore SIGINT forever so this process's group stays alive no
		// matter how many times it gets signaled; the test kills it
		// directly (SIGKILL, via t.Cleanup) once done.
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		_ = os.WriteFile(os.Getenv("HELPER_READY_FILE"), []byte("ready\n"), 0o644)
		// Block forever: signal.Notify above already changed SIGINT's
		// disposition so the runtime won't terminate the process on
		// delivery, whether or not anything ever reads ch (same "catch and
		// park until externally SIGKILLed" idiom as
		// TestIntegrationSupervisorStopHelperProcess in
		// test/integration/integration_test.go).
		select {}
	case "late":
		marker := os.Getenv("HELPER_MARKER_FILE")
		joinPgid, _ := strconv.Atoi(os.Getenv("HELPER_JOIN_PGID"))
		delayMS, _ := strconv.Atoi(os.Getenv("HELPER_JOIN_DELAY_MS"))
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT)
		time.Sleep(time.Duration(delayMS) * time.Millisecond)
		if err := syscall.Setpgid(0, joinPgid); err != nil {
			os.Exit(2)
		}
		<-ch
		_ = os.WriteFile(marker, []byte("interrupted\n"), 0o644)
		os.Exit(0)
	default:
		// Not invoked as a helper (a normal `go test` run) — no-op.
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
