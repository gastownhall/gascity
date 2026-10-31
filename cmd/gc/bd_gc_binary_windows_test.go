//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubInvokingExecutable(t *testing.T, path string) {
	t.Helper()
	previous := resolveInvokingExecutable
	resolveInvokingExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { resolveInvokingExecutable = previous })
}

func writeProgramFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveBdInvokingGCBinaryAcceptsAWindowsProgram pins the strict GC_BIN
// resolver on Windows, where os.Stat reports no execute permission bits: a
// mode-bit test refuses every gc.exe, which stops `gc init` of a bd-backed city
// before it begins.
func TestResolveBdInvokingGCBinaryAcceptsAWindowsProgram(t *testing.T) {
	t.Run("the running executable", func(t *testing.T) {
		running, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(running)
		if err != nil {
			t.Fatal(err)
		}
		got, err := resolveBdInvokingGCBinary()
		if err != nil {
			t.Fatalf("resolveBdInvokingGCBinary() refused the running executable %s: %v", running, err)
		}
		if got != want {
			t.Fatalf("resolveBdInvokingGCBinary() = %q, want %q", got, want)
		}
	})
	for _, name := range []string{"gc.exe", "GC.EXE"} {
		t.Run(name, func(t *testing.T) {
			path := writeProgramFixture(t, name)
			stubInvokingExecutable(t, path)
			want, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolveBdInvokingGCBinary()
			if err != nil {
				t.Fatalf("resolveBdInvokingGCBinary() refused %s: %v", path, err)
			}
			if got != want {
				t.Fatalf("resolveBdInvokingGCBinary() = %q, want %q", got, want)
			}
		})
	}
}

func TestResolveBdInvokingGCBinaryAcceptsExplicitImageWhenPATHEXTOmitsIt(t *testing.T) {
	t.Setenv("PATHEXT", ".PS1")
	for _, name := range []string{"gc.exe", "GC.COM"} {
		t.Run(name, func(t *testing.T) {
			path := writeProgramFixture(t, name)
			stubInvokingExecutable(t, path)
			want, err := filepath.EvalSymlinks(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolveBdInvokingGCBinary()
			if err != nil {
				t.Fatalf("resolveBdInvokingGCBinary() refused explicit executable image %s: %v", path, err)
			}
			if got != want {
				t.Fatalf("resolveBdInvokingGCBinary() = %q, want %q", got, want)
			}
		})
	}
}

func TestResolveBdInvokingGCBinaryRefusesWhatWindowsWillNotRun(t *testing.T) {
	for _, name := range []string{"gc", "gc.txt"} {
		t.Run(name, func(t *testing.T) {
			stubInvokingExecutable(t, writeProgramFixture(t, name))
			if got, err := resolveBdInvokingGCBinary(); err == nil || !strings.Contains(err.Error(), "is not an executable regular file") {
				t.Fatalf("resolveBdInvokingGCBinary() = (%q, %v), want the not-executable refusal", got, err)
			}
		})
	}
	t.Run("a directory named gc.exe", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "gc.exe")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stubInvokingExecutable(t, dir)
		if got, err := resolveBdInvokingGCBinary(); err == nil || !strings.Contains(err.Error(), "is not an executable regular file") {
			t.Fatalf("resolveBdInvokingGCBinary() = (%q, %v), want the not-executable refusal", got, err)
		}
	})
}

func TestRunnableGCBinaryRecognisesAWindowsProgram(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gc.exe")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want bool
	}{
		"gc.exe":                   {writeProgramFixture(t, "gc.exe"), true},
		"GC.EXE":                   {writeProgramFixture(t, "GC.EXE"), true},
		"gc":                       {writeProgramFixture(t, "gc"), false},
		"gc.txt":                   {writeProgramFixture(t, "gc.txt"), false},
		"a directory named gc.exe": {dir, false},
		"a missing gc.exe":         {filepath.Join(t.TempDir(), "gc.exe"), false},
	} {
		if got := runnableGCBinary(tc.path); got != tc.want {
			t.Errorf("runnableGCBinary(%s: %s) = %t, want %t", name, tc.path, got, tc.want)
		}
	}
}

// TestBestEffortGCBinaryPinsAWindowsGCOnPath is the Windows form of the PATH
// fallback in TestLegacyBdRunnersSurviveARemovedGCExecutable, whose fixture is
// an extensionless gc that Windows path search never finds.
func TestBestEffortGCBinaryPinsAWindowsGCOnPath(t *testing.T) {
	stubRemovedGCExecutable(t)
	onPath := writeProgramFixture(t, "gc.exe")
	t.Setenv("PATH", filepath.Dir(onPath))
	env := map[string]string{}
	pinBdGCEnvironmentBestEffort(env)
	if env["GC_BIN"] != onPath {
		t.Fatalf("GC_BIN = %q, want the gc.exe still installed on PATH %q", env["GC_BIN"], onPath)
	}
}
