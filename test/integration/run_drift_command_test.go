//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeDriftBinary writes an executable shell script standing in for
// gc and returns its path. `exec` in the body keeps the signal that kills
// a timed-out command on the real process, so the pipe closes with it.
func writeFakeDriftBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-gc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("writing fake binary: %v", err)
	}
	return path
}

// TestRunDriftCommand_DeadlineKillIsReportedAsSuch pins that a command the
// context deadline kills says so (with the elapsed time and the error), so
// the -1 exit code is no longer the only evidence of what happened.
func TestRunDriftCommand_DeadlineKillIsReportedAsSuch(t *testing.T) {
	bin := writeFakeDriftBinary(t, "echo partial output\nexec sleep 30")

	out, exitCode, elapsed := runDriftCommandWithin(t, 300*time.Millisecond, bin, os.Environ(), t.TempDir(), "start")

	if exitCode != -1 {
		t.Errorf("exit code = %d, want -1 for a deadline kill", exitCode)
	}
	if elapsed > 10*time.Second {
		t.Errorf("elapsed = %s, want the 300ms deadline to end the command promptly", elapsed)
	}
	if !strings.Contains(out, "partial output") {
		t.Errorf("command output was dropped:\n%s", out)
	}
	for _, want := range []string{"context deadline exceeded", "300ms", "signal: killed"} {
		if !strings.Contains(out, want) {
			t.Errorf("deadline-kill report missing %q:\n%s", want, out)
		}
	}
}

// TestRunDriftCommand_OtherNonZeroExitIsNotCalledADeadline pins that an
// ordinary failing exit is reported with its error and says the deadline
// did not fire.
func TestRunDriftCommand_OtherNonZeroExitIsNotCalledADeadline(t *testing.T) {
	bin := writeFakeDriftBinary(t, "echo boom\nexit 3")

	out, exitCode, _ := runDriftCommandWithin(t, 30*time.Second, bin, os.Environ(), t.TempDir(), "start")

	if exitCode != 3 {
		t.Errorf("exit code = %d, want 3", exitCode)
	}
	if strings.Contains(out, "context deadline exceeded") {
		t.Errorf("a plain exit was reported as a deadline kill:\n%s", out)
	}
	for _, want := range []string{"boom", "exit status 3", "deadline not exceeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("failure report missing %q:\n%s", want, out)
		}
	}
}

// TestRunDriftCommand_SuccessAddsNoDiagnostic pins that a clean exit leaves
// the command output untouched.
func TestRunDriftCommand_SuccessAddsNoDiagnostic(t *testing.T) {
	bin := writeFakeDriftBinary(t, "echo fine")

	out, exitCode, _ := runDriftCommandWithin(t, 30*time.Second, bin, os.Environ(), t.TempDir(), "start")

	if exitCode != 0 {
		t.Errorf("exit code = %d, want 0", exitCode)
	}
	if out != "fine\n" {
		t.Errorf("output = %q, want %q", out, "fine\n")
	}
}

// TestRunDriftCommand_FailureReportCarriesTheElapsedTime pins the elapsed-time
// clause of the diagnostic: a failing command's report says how long it ran
// before it failed, and the figure is the one the helper returns, so a restart
// that dies after half a minute reads differently from one that dies at once.
func TestRunDriftCommand_FailureReportCarriesTheElapsedTime(t *testing.T) {
	bin := writeFakeDriftBinary(t, "sleep 1\nexit 3")

	out, exitCode, elapsed := runDriftCommandWithin(t, 30*time.Second, bin, os.Environ(), t.TempDir(), "start")

	if exitCode != 3 {
		t.Fatalf("exit code = %d, want 3", exitCode)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("elapsed = %s, want about the 1s the command slept", elapsed)
	}
	if want := "after " + elapsed.Round(time.Millisecond).String(); !strings.Contains(out, want) {
		t.Errorf("failure report missing the elapsed time %q:\n%s", want, out)
	}
}

// TestDriftSystemdCommandTimeout_FitsALoadedRestartButStaysBounded pins both
// sides of the systemd call's own bound. It leaves room for the 31.1s restart a
// loaded gate host took on top of the default bound that the restart outran,
// and it is still a bound, so a command that really hangs fails within minutes
// rather than at the test binary's own timeout. The call site is not pinned
// here: TestStartDrift_SystemdManaged_RestartsToNewBuildID is the only test that
// reaches it, and it skips where there is no user systemd.
func TestDriftSystemdCommandTimeout_FitsALoadedRestartButStaysBounded(t *testing.T) {
	const (
		loadedRestart = 31100 * time.Millisecond // gate tracker ga-dhnv01xi
		longestBound  = 5 * time.Minute
	)
	got := driftSystemdCommandTimeout

	if got < driftCommandTimeout+loadedRestart {
		t.Errorf("driftSystemdCommandTimeout = %s, want at least the default %s plus the %s restart a loaded gate host took",
			got, driftCommandTimeout, loadedRestart)
	}
	if got > longestBound {
		t.Errorf("driftSystemdCommandTimeout = %s, want at most %s, so a command that really hangs still fails", got, longestBound)
	}
}
