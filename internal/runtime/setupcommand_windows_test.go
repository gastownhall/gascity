//go:build windows

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunSetupCommandRunsWithoutPosixShellOnPath is the acceptance test for the
// supervisor path: with no PATH at all, so neither sh.exe nor bash.exe can be
// resolved and no Git Bash or WSL launcher is reachable, a setup command must
// still run, still report its working directory, and still fail with its own
// exit code.
//
// RunSetupCommand is the lifecycle runner every host-side provider delegates
// to, so a Windows host that needs a POSIX shell here does not have a working
// supervisor no matter what the rest of the suite reports.
func TestRunSetupCommandRunsWithoutPosixShellOnPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", "")
	t.Setenv("PATHEXT", "")

	for _, name := range []string{"sh", "sh.exe", "bash", "bash.exe", "wsl.exe"} {
		if path, err := exec.LookPath(name); err == nil {
			t.Fatalf("%s is still resolvable at %s; the test is not proving anything", name, path)
		}
	}

	if err := RunSetupCommand(context.Background(), fixturePrintCwdToFile, map[string]string{
		"GC_DIR": dir,
	}, 30*time.Second); err != nil {
		t.Fatalf("setup command failed with no POSIX shell on PATH: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("setup command did not run in GC_DIR: %v", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(data)), dir) {
		t.Errorf("working directory = %q, want %q", strings.TrimSpace(string(data)), dir)
	}

	err = RunSetupCommand(context.Background(), fixtureBothStreamsExit3, nil, 30*time.Second)
	if err == nil {
		t.Fatal("a setup command exiting 3 must fail with no POSIX shell on PATH")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %v (%T), want an *exec.ExitError so error classification keeps working", err, err)
	}
	if got := exitErr.ExitCode(); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
}

// TestRunSetupCommandReportsMissingInterpreter pins that an unresolvable
// interpreter is reported rather than silently skipped. A setup command that
// did not run must never look like one that succeeded.
func TestRunSetupCommandReportsMissingInterpreter(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("SystemRoot", t.TempDir())

	err := RunSetupCommand(context.Background(), "exit 0", nil, 5*time.Second)
	if err == nil {
		t.Fatal("a setup command with no interpreter must fail")
	}
	if !strings.Contains(err.Error(), "no PowerShell interpreter found") {
		t.Fatalf("error = %v, want the missing interpreter to be named", err)
	}
}
