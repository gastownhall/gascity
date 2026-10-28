//go:build unix

package pidutil

// Reading another process's argv needs /proc, kern.procargs2, or ps. Windows
// has none of them -- Cmdline reports ErrUnsupported there rather than
// guessing -- so these cases, and the ps-stub fixtures they rely on, only
// mean anything on Unix. The Windows contract is pinned separately in
// pidutil_windows_test.go: unreadable argv must surface as an error, never as
// a clean 'not my process'.
import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// spawnSleeper starts a long-lived child and returns its pid. argv is exactly
// ["sleep","60"], which is what both the /proc and ps paths must report.
func spawnSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Give the exec a moment so the argv is the sleeper's, not the shell's.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if argv, err := Cmdline(cmd.Process.Pid); err == nil && len(argv) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cmd.Process.Pid
}

// TestAliveWithCmdline_RejectsLivePIDWithNonMatchingArgv is the defect, stated
// directly: a live process whose argv does NOT match must be rejected. Off Linux
// this returned true, which is what let an unrelated recycled PID pass as the
// caller's own poller.
func TestAliveWithCmdline_RejectsLivePIDWithNonMatchingArgv(t *testing.T) {
	pid := spawnSleeper(t)

	got, err := AliveWithCmdline(pid, func(argv []string) bool {
		return ArgvContainsSequence(argv, "definitely-not-in-this-argv")
	})
	if err != nil {
		t.Fatalf("AliveWithCmdline(%d, non-matching): %v", pid, err)
	}

	if got {
		t.Fatalf("AliveWithCmdline(%d, non-matching) = true on %s; an unrelated live PID passes as the caller's own process", pid, runtime.GOOS)
	}
}

// TestAliveWithCmdline_AcceptsMatchingArgv is the over-correction guard: the
// check must still say yes to the real process. Passes before and after.
func TestAliveWithCmdline_AcceptsMatchingArgv(t *testing.T) {
	pid := spawnSleeper(t)

	got, err := AliveWithCmdline(pid, func(argv []string) bool {
		return ArgvContainsSequence(argv, "sleep", "60")
	})
	if err != nil {
		t.Fatalf("AliveWithCmdline(%d, matching): %v", pid, err)
	}

	if !got {
		argv, cmdErr := Cmdline(pid)
		t.Fatalf("AliveWithCmdline(%d, matching) = false; argv=%q err=%v", pid, argv, cmdErr)
	}
}

// TestCmdline_ReturnsArgvOnThisHost is the regression test for the cause rather
// than the symptom: Cmdline must produce argv on the host it runs on. It
// returned an error on every non-Linux host, which is what forced the
// short-circuit above.
func TestCmdline_ReturnsArgvOnThisHost(t *testing.T) {
	argv, err := Cmdline(os.Getpid())
	if err != nil {
		t.Fatalf("Cmdline(self) on %s: %v", runtime.GOOS, err)
	}
	if len(argv) == 0 {
		t.Fatalf("Cmdline(self) on %s returned no argv", runtime.GOOS)
	}
}

// TestCmdline_FailsClosedWhenUnreadable covers the direction that matters for
// safety here. A live process whose argv cannot be read must NOT be reported as
// a plain non-match: the caller would then assume no poller is running and
// start a second one over the top of the first. The unreadable case is reported
// as an error so the caller can refuse instead.
//
// PID 1 is the probe target rather than a stubbed ps on PATH: Cmdline reads
// /proc directly on linux and kern.procargs2 directly on darwin for any live,
// readable PID, so a ps stub never affected self's argv on either platform —
// on darwin specifically, #5176 added the sysctl path, which made this
// untestable there regardless of the stub. PID 1's argv, unlike this
// process's own, is genuinely unreadable to an unprivileged caller on
// darwin, which is the real permission boundary this test needs. Skip where
// it IS readable (e.g. linux, where /proc/1/cmdline is typically
// world-readable) rather than assert something false.
func TestCmdline_FailsClosedWhenUnreadable(t *testing.T) {
	if !Alive(1) {
		t.Skip("PID 1 is not an inspectable live process here; the live-but-unreadable case cannot be staged this way")
	}
	if _, err := Cmdline(1); err == nil {
		t.Skip("PID 1 argv is readable in this environment; cannot exercise the fail-closed path this way")
	}
	if runtime.GOOS == "darwin" {
		t.Skip("on darwin kern.procargs2 answers directly, so the ps stub cannot make argv unreadable")
	}

	got, err := AliveWithCmdline(1, func([]string) bool { return true })
	if got {
		t.Fatal("AliveWithCmdline(1, ...) = true with unreadable argv")
	}
	if err == nil {
		t.Fatal("AliveWithCmdline(1, ...) returned no error with unreadable argv; a live-but-unidentifiable PID must not read as a plain non-match")
	}
}

// TestPSCmdlineParsesOwnArgv exercises the ps parse path directly. Calling
// psCmdline bypasses Cmdline's /proc shortcut, so the parser this PR adds
// gets real coverage on linux runners too — otherwise it runs nowhere in CI.
func TestPSCmdlineParsesOwnArgv(t *testing.T) {
	argv, err := psCmdline(os.Getpid())
	if err != nil {
		t.Fatalf("psCmdline(self) on %s: %v", runtime.GOOS, err)
	}
	if len(argv) == 0 || !strings.Contains(filepath.Base(argv[0]), "pidutil") {
		t.Fatalf("psCmdline(self) = %q, want test binary argv", argv)
	}
}

// TestPSCmdlineIsBounded mirrors the existing zombie-probe guard: a hung ps must
// not stall a caller that runs on a reconciler tick.
func TestPSCmdlineIsBounded(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv("GC_PIDUTIL_PS_TIMEOUT", "1s")

	start := time.Now()
	_, _ = psCmdline(os.Getpid())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("psCmdline took %s, want a bounded timeout", elapsed)
	}
}
