//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file pins F-C against the PRODUCTION writer, not an injected one.
//
// The unit tests for F-C hand doHookClaim an io.Writer that returns
// syscall.EPIPE as an ordinary error. That is not what a closed tool pipe does
// to a real process. Go's runtime raises SIGPIPE when a write to file
// descriptor 1 or 2 fails with EPIPE, and the default disposition TERMINATES
// the program — so on real stdout the process dies inside the write and the
// unwind never runs. The claim stays parked, which is precisely the incident
// F-C exists to end.
//
// The fix is ignoreSIGPIPE() at startup: with SIGPIPE ignored, the runtime lets
// the write return EPIPE to Go code instead of killing the process. The test
// below runs the real path over a real pipe whose read end is closed, in a
// subprocess, and the control runs the same path WITHOUT the fix to show the
// difference is load-bearing rather than assumed.

const sigpipeUnwindHelperName = "hook-claim-sigpipe-unwind-helper"

// sigpipeUnwindHelperArgs recognizes the re-exec of this test binary as the
// child that writes a claim result to a closed pipe.
func sigpipeUnwindHelperArgs(args []string) (mode, markerPath string, ok bool) {
	for index, arg := range args {
		if arg == "--" && index+4 == len(args) && args[index+1] == sigpipeUnwindHelperName {
			return args[index+2], args[index+3], true
		}
	}
	return "", "", false
}

// runSigpipeUnwindHelper is the child. It claims one bead through the real
// claim path with os.Stdout as the result writer, and records the release in a
// marker file so the parent can tell an unwound claim from a parked one.
//
// mode "ignore" applies the production startup fix; mode "default" deliberately
// does not, which is the control.
func runSigpipeUnwindHelper(mode, markerPath string) {
	if mode == "ignore" {
		ignoreSIGPIPE()
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return turnBoundRoutedWork, nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		},
		Release: func(_ context.Context, _ string, _ []string, beadID, _ string) (bool, error) {
			// The marker IS the assertion: it exists only if the unwind ran.
			_ = os.WriteFile(markerPath, []byte("released "+beadID), 0o644)
			return true, nil
		},
		EmitClaimReleased:        func(hookClaimReleaseRecord) {},
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
		PublishRunMap:            func(string, string, ...string) error { return nil },
		StampWorkMeta:            func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		ResolveWorkBranch:        func(string) string { return "" },
	}
	// os.Stdout, not a buffer: the whole point is the real file descriptor.
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee:     "worker-1",
		RouteTargets: []string{"worker"},
		JSON:         true,
	}, ops, os.Stdout, os.Stderr)
	os.Exit(code)
}

// runSigpipeUnwindChild re-execs this test binary with its stdout wired to a
// pipe whose read end is closed before the child writes, and reports the exit
// code, whether the release marker appeared, and any terminating signal.
func runSigpipeUnwindChild(t *testing.T, mode string) (exitCode int, released bool, signaled syscall.Signal) {
	t.Helper()
	markerPath := filepath.Join(t.TempDir(), "released")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	// Both children re-exec the same helper test; the mode argument selects
	// whether the startup fix is applied.
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestFCUnwindSurvivesARealClosedPipe$",
		"--", sigpipeUnwindHelperName, mode, markerPath)
	cmd.Env = append(os.Environ(), "GC_SIGPIPE_UNWIND_CHILD=1")
	cmd.Stdout = writer
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	// Close BOTH ends in the parent: the write end so the child owns the only
	// copy, and the read end so the child's first write finds no reader. That is
	// the provider's closed tool pipe.
	_ = writer.Close()
	_ = reader.Close()

	err = cmd.Wait()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		exitCode = 0
	case errors.As(err, &exitErr):
		exitCode = exitErr.ExitCode()
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			signaled = status.Signal()
		}
	default:
		t.Fatalf("waiting for child: %v", err)
	}
	if _, statErr := os.Stat(markerPath); statErr == nil {
		released = true
	}
	return exitCode, released, signaled
}

// TestFCUnwindSurvivesARealClosedPipe is the production-path proof for F-C: a
// claim whose result cannot be delivered over a REAL closed stdout is released,
// and the process exits 1 rather than dying mid-write.
func TestFCUnwindSurvivesARealClosedPipe(t *testing.T) {
	if mode, markerPath, ok := sigpipeUnwindHelperArgs(os.Args); ok {
		runSigpipeUnwindHelper(mode, markerPath)
		return
	}

	exitCode, released, signaled := runSigpipeUnwindChild(t, "ignore")
	if signaled == syscall.SIGPIPE {
		t.Fatalf("child died from SIGPIPE; the claim path cannot unwind a claim it was killed inside of")
	}
	if !released {
		t.Errorf("the claim was NOT released after an undeliverable result on a real pipe; it is parked exactly as the incident describes")
	}
	if exitCode != 1 {
		t.Errorf("child exit = %d, want 1", exitCode)
	}
}

// TestRealClosedPipeKillsAnUnprotectedProcess is the differently-failing
// control, and it is the reason this file exists.
//
// It runs the identical claim over the identical closed pipe WITHOUT the
// startup fix and asserts the process is killed by SIGPIPE with no release.
// That is the pre-fix behavior, so it demonstrates the fix is load-bearing
// rather than decorative — and if a future change makes SIGPIPE non-fatal by
// some other route, this test says so instead of quietly agreeing.
func TestRealClosedPipeKillsAnUnprotectedProcess(t *testing.T) {
	if _, _, ok := sigpipeUnwindHelperArgs(os.Args); ok {
		// The helper branch belongs to the test above; never recurse here.
		return
	}
	exitCode, released, signaled := runSigpipeUnwindChild(t, "default")
	if signaled != syscall.SIGPIPE {
		t.Fatalf("unprotected child was not killed by SIGPIPE (exit=%d signal=%v); ignoreSIGPIPE would then be protecting against nothing, so verify the premise before trusting the fix", exitCode, signaled)
	}
	if released {
		t.Error("unprotected child released the claim; it should have died inside the write")
	}
}

// TestMainIgnoresSIGPIPE pins the WIRING. The helper above calls ignoreSIGPIPE
// explicitly, which proves the mechanism; this proves production actually
// installs it, so the fix cannot be left defined-but-uncalled.
//
// It checks mainExitCode rather than main because the exit-bypass census
// requires main's body to be exactly the os.Exit around mainExitCode — so
// mainExitCode is where pre-dispatch startup work has to live.
func TestMainIgnoresSIGPIPE(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	body := string(source)
	start := strings.Index(body, "\nfunc mainExitCode(")
	if start < 0 {
		t.Fatal("could not find func mainExitCode in main.go")
	}
	end := strings.Index(body[start+1:], "\nfunc ")
	if end < 0 {
		end = len(body) - start - 1
	}
	if !strings.Contains(body[start:start+1+end], "ignoreSIGPIPE()") {
		t.Fatal("mainExitCode does not call ignoreSIGPIPE(); a closed stdout would kill gc mid-write and no claim unwind could run")
	}
}
