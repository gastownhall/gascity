// Package dolt_test validates that runtime.sh's run_bounded helper
// honors its own documented contract (SIGTERM, brief grace period,
// then SIGKILL) on every fallback path — including the python3
// fallback used when neither timeout nor gtimeout is on PATH, which
// previously escalated straight to SIGKILL. See gascity#4823: the
// mismatch let a bounded `dolt backup sync` be killed without any
// chance to run its own signal handler, leaking unreferenced backup
// archives (dolt has no prune verb).
package dolt_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runRunBoundedUnderPython3Fallback sources runtime.sh with a PATH
// that exposes only python3 (no timeout/gtimeout), so run_bounded is
// forced onto the fallback branch under test, then invokes
// `run_bounded 1 python3 childScript` against the given child script.
//
// The child is itself a python3 script (not a shell script wrapping
// a blocking `sleep`) because a shell blocked in wait() on a
// foreground child defers pending trap handlers until that child
// returns — an artifact of shell signal delivery, not of run_bounded.
// A single process installing its own Python signal handler mirrors
// how a real target like `dolt` receives and reacts to signals.
func runRunBoundedUnderPython3Fallback(t *testing.T, childScript string, extraEnv ...string) (int, string) {
	t.Helper()

	python3Path, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed; cannot exercise run_bounded's python3 fallback")
	}
	bin := t.TempDir()
	if err := os.Symlink(python3Path, filepath.Join(bin, "python3")); err != nil {
		t.Fatalf("symlink python3: %v", err)
	}
	hostSh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("LookPath(sh): %v", err)
	}
	if err := os.Symlink(hostSh, filepath.Join(bin, "sh")); err != nil {
		t.Fatalf("symlink sh: %v", err)
	}

	root := repoRoot(t)
	cityPath := t.TempDir()
	cmd := exec.Command("sh", "-c",
		`. "$GC_PACK_DIR/assets/scripts/runtime.sh"; run_bounded 1 python3 `+shellQuote(childScript))
	cmd.Env = append(filteredEnv("GC_CITY_PATH", "GC_PACK_DIR", "GC_DOLT_PORT", "PATH"),
		"GC_CITY_PATH="+cityPath,
		"GC_PACK_DIR="+root,
		"GC_DOLT_PORT=4406",
		"PATH="+bin,
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), string(out)
	}
	t.Fatalf("running run_bounded: %v\noutput:\n%s", err, out)
	return 0, ""
}

// TestRunBoundedPython3FallbackSendsSigtermBeforeKill is the
// regression guard for gascity#4823: on timeout, the python3 fallback
// must give the child a chance to catch SIGTERM and exit gracefully,
// not jump straight to SIGKILL. The child here installs a SIGTERM
// handler and writes a marker file from it; under the pre-fix
// `subprocess.run(..., timeout=...)` implementation (bare
// `process.kill()` on expiry) the handler never runs and the marker
// is never written.
// The 1s bound below is production input to run_bounded, not this test's
// own hang-detector timer (TESTING.md: a timeout the test feeds INTO the
// code under test is never migrated to a hang budget), so the blanket
// >=10s "Test deadline rule" for timers racing an exec start does not
// apply to it — enlarging it would hide the very race gascity#4823 exists
// to catch. run_bounded starts its timer at Popen, so python3 interpreter
// startup is counted against the bound, and a starved host can starve the
// child before signal.signal ever runs. The READY marker, written
// immediately after the handler is installed and before the child
// sleeps, is what keeps the assertion honest instead: an attempt where
// READY never appears proved nothing about the SIGTERM contract (the
// child never reached the handler within the bound) and is retried
// rather than treated as a regression.
func TestRunBoundedPython3FallbackSendsSigtermBeforeKill(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "sigterm-received")
	ready := filepath.Join(dir, "ready")
	child := filepath.Join(dir, "child.py")
	writeExecutable(t, child, `import os
import signal
import sys
import time


def handler(signum, frame):
    with open(os.environ["MARKER_FILE"], "w") as f:
        f.write("caught\n")
    sys.exit(0)


signal.signal(signal.SIGTERM, handler)
with open(os.environ["READY_MARKER"], "w") as f:
    f.write("ready\n")
time.sleep(10)
`)

	const maxAttempts = 3
	var exitCode int
	var out string
	reachedReady := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_ = os.Remove(marker)
		_ = os.Remove(ready)
		exitCode, out = runRunBoundedUnderPython3Fallback(t, child, "MARKER_FILE="+marker, "READY_MARKER="+ready)
		if _, err := os.Stat(ready); err == nil {
			reachedReady = true
			break
		}
	}
	if !reachedReady {
		t.Fatalf("child never reached its SIGTERM handler installation within the bound after %d attempts (readiness marker never appeared); likely host load starved python3 interpreter startup, not a SIGTERM regression\noutput:\n%s", maxAttempts, out)
	}

	if exitCode != 124 {
		t.Fatalf("run_bounded exit code = %d, want 124 (timeout)\noutput:\n%s", exitCode, out)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("child was ready and still never received SIGTERM (marker file missing): %v\noutput:\n%s", err, out)
	}
	if string(got) != "caught\n" {
		t.Fatalf("marker file contents = %q, want %q", got, "caught\n")
	}
}

// TestRunBoundedPython3FallbackEscalatesToSigkillAfterGrace confirms
// the other half of the contract still holds: a child that ignores
// SIGTERM is killed shortly after the grace period, not left running
// forever.
func TestRunBoundedPython3FallbackEscalatesToSigkillAfterGrace(t *testing.T) {
	dir := t.TempDir()
	doneMarker := filepath.Join(dir, "still-running")
	child := filepath.Join(dir, "child.py")
	writeExecutable(t, child, `import os
import signal
import time

signal.signal(signal.SIGTERM, signal.SIG_IGN)
with open(os.environ["DONE_MARKER"], "w") as f:
    f.write("started\n")
time.sleep(30)
`)

	start := time.Now()
	exitCode, out := runRunBoundedUnderPython3Fallback(t, child, "DONE_MARKER="+doneMarker)
	elapsed := time.Since(start)

	if exitCode != 124 {
		t.Fatalf("run_bounded exit code = %d, want 124 (timeout)\noutput:\n%s", exitCode, out)
	}
	if _, err := os.ReadFile(doneMarker); err != nil {
		t.Fatalf("child never started: %v\noutput:\n%s", err, out)
	}
	// 1s timeout + up to 2s grace; allow generous scheduling slack
	// while still proving the child didn't run the full 30s sleep.
	if elapsed > 10*time.Second {
		t.Fatalf("run_bounded took %s to return; expected escalation to SIGKILL well under 10s", elapsed)
	}
}

// TestRunBoundedPython3FallbackPassesThroughOutputAndExitCode covers the
// non-timeout path: the rewrite swapped buffered capture for inherited
// fds, and on stock macOS this fallback is run_bounded's only
// implementation, so a child that exits normally must still have its
// output and exit status reach the caller.
func TestRunBoundedPython3FallbackPassesThroughOutputAndExitCode(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child.py")
	writeExecutable(t, child, `import sys

sys.stdout.write("to-stdout\n")
sys.stderr.write("to-stderr\n")
sys.exit(3)
`)

	exitCode, out := runRunBoundedUnderPython3Fallback(t, child)

	if exitCode != 3 {
		t.Fatalf("run_bounded exit code = %d, want 3 (child's own status)\noutput:\n%s", exitCode, out)
	}
	if !strings.Contains(out, "to-stdout") || !strings.Contains(out, "to-stderr") {
		t.Fatalf("child output not passed through; got:\n%s", out)
	}
}
