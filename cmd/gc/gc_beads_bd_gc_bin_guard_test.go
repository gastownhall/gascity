package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gcBinGuardScriptPath returns the bd pack provider script under test.
func gcBinGuardScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
}

// TestRequireExecutableGCBin exercises the require_executable_gc_bin guard in
// gc-beads-bd.sh. The regression it guards: a long-running supervisor launched
// from a gc binary that was later removed (e.g. `brew unlink`) keeps exporting
// its original path as GC_BIN. The script used to hand that path to every gc
// helper call and died deep inside an operation with a bare "No such file or
// directory", after having already fallen back to legacy layout derivation.
func TestRequireExecutableGCBin(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell-function test")
	}
	scriptBytes, err := os.ReadFile(gcBinGuardScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "require_executable_gc_bin")

	dir := t.TempDir()
	executable := filepath.Join(dir, "gc")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notExecutable := filepath.Join(dir, "gc-not-exec")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "removed", "gc")

	cases := []struct {
		name   string
		gcBin  string
		wantOK bool
	}{
		{name: "unset", gcBin: "", wantOK: true},
		{name: "executable", gcBin: executable, wantOK: true},
		{name: "missing", gcBin: missing, wantOK: false},
		{name: "not_executable", gcBin: notExecutable, wantOK: false},
		{name: "directory", gcBin: dir, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"require_executable_gc_bin\n" +
				"echo reached\n"
			env := append(os.Environ(), "GC_BIN="+tc.gcBin)
			stdout, stderr, runErr := runGCBeadsBdCommand(t, env, "sh", "-c", script)
			if tc.wantOK {
				if runErr != nil || !strings.Contains(stdout, "reached") {
					t.Fatalf("guard rejected GC_BIN=%q: err=%v stdout=%q stderr=%q", tc.gcBin, runErr, stdout, stderr)
				}
				return
			}
			if runErr == nil || strings.Contains(stdout, "reached") {
				t.Fatalf("guard accepted unusable GC_BIN=%q: stdout=%q stderr=%q", tc.gcBin, stdout, stderr)
			}
			assertGCBinGuardMessage(t, stderr, tc.gcBin)
		})
	}
}

// TestGCBeadsBDFailsClosedOnStaleGCBin runs the whole provider script with a
// GC_BIN that no longer exists and asserts it stops before touching the city:
// no silent fallback to legacy layout derivation, no state directories, and no
// PATH-resolved gc of an unknown version standing in for the missing one.
func TestGCBeadsBDFailsClosedOnStaleGCBin(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell script test")
	}
	cityDir := t.TempDir()
	pathDir := t.TempDir()
	pathGCLog := filepath.Join(pathDir, "gc-invoked.log")
	// A gc on PATH that records any invocation: the script must not use it
	// in place of the missing GC_BIN.
	fakeGC := "#!/bin/sh\necho \"$@\" >>" + pathGCLog + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(pathDir, "gc"), []byte(fakeGC), 0o755); err != nil {
		t.Fatal(err)
	}
	staleGCBin := filepath.Join(t.TempDir(), "opt", "homebrew", "bin", "gc")

	for _, op := range []string{"start", "health", "stop"} {
		t.Run(op, func(t *testing.T) {
			env := append(os.Environ(),
				"GC_BIN="+staleGCBin,
				"GC_CITY_PATH="+cityDir,
				"GC_DOLT=",
				"GC_BEADS_PROVIDER_OWNED=",
				"PATH="+pathDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			)
			_, stderr, runErr := runGCBeadsBdCommand(t, env, "sh", gcBinGuardScriptPath(t), op)
			exitErr := &exec.ExitError{}
			ok := errors.As(runErr, &exitErr)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("gc-beads-bd.sh %s with stale GC_BIN: err=%v, want exit 1; stderr=%q", op, runErr, stderr)
			}
			assertGCBinGuardMessage(t, stderr, staleGCBin)
			for _, sub := range []string{".gc", ".beads"} {
				if _, err := os.Stat(filepath.Join(cityDir, sub)); !os.IsNotExist(err) {
					t.Fatalf("gc-beads-bd.sh %s created %s before failing on stale GC_BIN (stat err=%v)", op, sub, err)
				}
			}
			if _, err := os.Stat(pathGCLog); !os.IsNotExist(err) {
				logged, _ := os.ReadFile(pathGCLog)
				t.Fatalf("gc-beads-bd.sh %s fell back to gc on PATH: %q", op, logged)
			}
		})
	}
}

func assertGCBinGuardMessage(t *testing.T, stderr, gcBin string) {
	t.Helper()
	for _, want := range []string{
		"GC_BIN=" + gcBin,
		"removed or upgraded",
		"gc supervisor install",
		"gc supervisor stop",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
}
