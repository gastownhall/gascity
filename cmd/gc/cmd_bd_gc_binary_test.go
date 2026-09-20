package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this file drive resolveBdInvokingGCBinary itself instead of
// stubbing it out at a call site, so the resolver's own failure modes — an
// executable path that cannot be inspected, a non-absolute one, and one that is
// not an executable regular file — are covered directly. They lock the
// degrade-rather-than-fail-closed posture: pinning an unverified path is
// strictly better than letting an ambient GC_BIN through, and far better than
// failing every bd operation for the life of the process.

// writeGCBinaryFixture writes an executable stand-in for a gc binary.
func writeGCBinaryFixture(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubInvokingGCExecutable points the os.Executable seam at a fixture, pins
// PATH to pathDir so the last resolution rung is deterministic instead of
// depending on the host's own gc installation, and captures the degradation
// warnings the resolver logs.
func stubInvokingGCExecutable(t *testing.T, executable string, resolveErr error, pathDir string) *bytes.Buffer {
	t.Helper()
	oldResolve := resolveInvokingExecutable
	resolveInvokingExecutable = func() (string, error) { return executable, resolveErr }
	t.Cleanup(func() { resolveInvokingExecutable = oldResolve })
	t.Setenv("PATH", pathDir)
	return captureDegradeWarnings(t)
}

// captureDegradeWarnings clears the per-rung warning guard and captures what the
// resolver logs while a test runs.
func captureDegradeWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	bdGCBinaryDegradeWarnedRungs.Clear()
	t.Cleanup(func() { bdGCBinaryDegradeWarnedRungs.Clear() })
	warnings := &bytes.Buffer{}
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(warnings)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	return warnings
}

// TestWarnDegradedBdGCBinaryReportsEachRungOnce locks the scope of the flood
// guard. Repeating one rung stays quiet, because resolution runs again for every
// bd subprocess and the condition does not change between them. But a process
// whose degradation changes rung — an upgrade window that later becomes an
// uninstall — must still report the condition actually in effect instead of
// leaving the operator with only the milder first line.
func TestWarnDegradedBdGCBinaryReportsEachRungOnce(t *testing.T) {
	warnings := captureDegradeWarnings(t)

	degraded := errors.New("canonicalize invoking gc executable")
	warnDegradedBdGCBinary("%v; pinning the uncanonicalized invoking path %q", degraded, "/opt/gc/bin/gc")
	warnDegradedBdGCBinary("%v; pinning the uncanonicalized invoking path %q", degraded, "/opt/gc/bin/gc")
	if got := strings.Count(warnings.String(), "uncanonicalized invoking path"); got != 1 {
		t.Errorf("one repeated rung warned %d times, want 1: per-subprocess resolution must not flood stderr", got)
	}

	warnDegradedBdGCBinary("%v; pinning %q from PATH", errors.New("stat invoking gc executable"), "/usr/bin/gc")
	if !strings.Contains(warnings.String(), "from PATH") {
		t.Errorf("warnings = %q, want the later PATH rung reported too: an earlier, milder degradation must not silence it", warnings.String())
	}
}

func TestResolveBdInvokingGCBinaryCanonicalizesThroughSymlink(t *testing.T) {
	target := writeGCBinaryFixture(t, filepath.Join(t.TempDir(), "gc"))
	link := filepath.Join(t.TempDir(), "gc-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	warnings := stubInvokingGCExecutable(t, link, nil, t.TempDir())

	got, err := resolveBdInvokingGCBinary()
	if err != nil {
		t.Fatalf("resolveBdInvokingGCBinary() error = %v, want the canonical target", err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("resolved gc binary = %q, want canonical %q", got, want)
	}
	if warnings.Len() != 0 {
		t.Errorf("verified resolution warned %q, want silence: the strict rung is the expected path", warnings.String())
	}
}

// TestResolveBdInvokingGCBinaryDegradesToInvokingPathWhenAbsent covers the
// operational case the strict resolver turned into an outage: a gc binary
// removed or replaced out from under a live process. os.Executable still
// reports the original absolute path (Go strips the kernel's " (deleted)"
// suffix), so EvalSymlinks is the step that fails. The pin degrades to that
// path — which is also where an rm-then-copy installer is about to put the new
// binary — rather than failing every bd command until the process restarts.
func TestResolveBdInvokingGCBinaryDegradesToInvokingPathWhenAbsent(t *testing.T) {
	pathDir := t.TempDir()
	onPATH := writeGCBinaryFixture(t, filepath.Join(pathDir, "gc"))
	absent := filepath.Join(t.TempDir(), "gc")
	warnings := stubInvokingGCExecutable(t, absent, nil, pathDir)

	got, err := resolveBdInvokingGCBinary()
	if err != nil {
		t.Fatalf("resolveBdInvokingGCBinary() error = %v, want a degraded pin: a removed gc binary must not fail every bd operation", err)
	}
	if got != absent {
		t.Errorf("resolved gc binary = %q, want the uncanonicalized invoking path %q (PATH's %q is a weaker rung)", got, absent, onPATH)
	}
	if !strings.Contains(warnings.String(), "uncanonicalized invoking path") {
		t.Errorf("warnings = %q, want the degraded pin reported", warnings.String())
	}

	env := map[string]string{"GC_BIN": "/tmp/ambient-gc"}
	if err := pinBdGCEnvironment(env); err != nil {
		t.Fatalf("pinBdGCEnvironment() error = %v, want the degraded pin applied", err)
	}
	if env["GC_BIN"] != absent {
		t.Errorf("pinned GC_BIN = %q, want %q: the ambient value must still be displaced", env["GC_BIN"], absent)
	}
}

func TestResolveBdInvokingGCBinaryFallsBackToPATHWhenNotAbsolute(t *testing.T) {
	pathDir := t.TempDir()
	onPATH := writeGCBinaryFixture(t, filepath.Join(pathDir, "gc"))
	warnings := stubInvokingGCExecutable(t, "gc", nil, pathDir)

	got, err := resolveBdInvokingGCBinary()
	if err != nil {
		t.Fatalf("resolveBdInvokingGCBinary() error = %v, want the PATH rung", err)
	}
	if got != onPATH {
		t.Errorf("resolved gc binary = %q, want %q from PATH", got, onPATH)
	}
	if !strings.Contains(warnings.String(), "from PATH") {
		t.Errorf("warnings = %q, want the PATH fallback reported", warnings.String())
	}
}

// TestResolveBdInvokingGCBinaryFallsBackToPATHWhenNotExecutable pins the one
// case where the uncanonicalized invoking path is *not* a usable rung: the path
// was inspected successfully and is not something a child can exec, so falling
// back to it would re-select the same file. PATH is the only rung left.
func TestResolveBdInvokingGCBinaryFallsBackToPATHWhenNotExecutable(t *testing.T) {
	pathDir := t.TempDir()
	onPATH := writeGCBinaryFixture(t, filepath.Join(pathDir, "gc"))
	notExecutable := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(notExecutable, []byte("not executable\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	warnings := stubInvokingGCExecutable(t, notExecutable, nil, pathDir)

	got, err := resolveBdInvokingGCBinary()
	if err != nil {
		t.Fatalf("resolveBdInvokingGCBinary() error = %v, want the PATH rung", err)
	}
	if got != onPATH {
		t.Errorf("resolved gc binary = %q, want %q from PATH: a non-executable file is not a usable pin", got, onPATH)
	}
	if !strings.Contains(warnings.String(), "not an executable regular file") {
		t.Errorf("warnings = %q, want the rejected candidate named", warnings.String())
	}
}

// TestResolveBdInvokingGCBinaryErrorsWhenNoCandidateResolves keeps the hard
// error for the one case the degradation ladder cannot cover: no rung produced
// a path at all. Pinning is the point of this helper, so callers must fail
// rather than silently hand an ambient GC_BIN to a bd child.
func TestResolveBdInvokingGCBinaryErrorsWhenNoCandidateResolves(t *testing.T) {
	sentinel := errors.New("no executable for this process")
	warnings := stubInvokingGCExecutable(t, "", sentinel, t.TempDir())

	got, err := resolveBdInvokingGCBinary()
	if err == nil {
		t.Fatalf("resolveBdInvokingGCBinary() = %q, want an error when neither the invoking executable nor PATH resolves", got)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap the underlying resolution failure %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("error = %v, want it to report the exhausted PATH rung too", err)
	}
	if warnings.Len() != 0 {
		t.Errorf("warnings = %q, want silence: nothing was pinned, so nothing degraded", warnings.String())
	}

	env := map[string]string{}
	if err := pinBdGCEnvironment(env); err == nil {
		t.Fatalf("pinBdGCEnvironment() = nil, want the resolution failure surfaced")
	}
	if _, ok := env["GC_BIN"]; ok {
		t.Errorf("pinBdGCEnvironment set GC_BIN = %q on failure, want it left alone", env["GC_BIN"])
	}
}
