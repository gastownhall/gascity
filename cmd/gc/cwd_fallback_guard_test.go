package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveImplicitCWD_NonTerminalRefuses(t *testing.T) {
	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	dir, err := resolveImplicitCWD()
	if err == nil {
		t.Fatalf("resolveImplicitCWD() returned nil error, dir = %q; want an error", dir)
	}
	if !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatalf("error = %q; want it to mention the non-interactive-terminal reason", err.Error())
	}
	if dir != "" {
		t.Fatalf("dir = %q; want empty on error", dir)
	}
}

func TestResolveImplicitCWD_TerminalReturnsCWD(t *testing.T) {
	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	t.Chdir(t.TempDir())
	realWant, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	dir, err := resolveImplicitCWD()
	if err != nil {
		t.Fatalf("resolveImplicitCWD() error = %v; want nil", err)
	}
	if dir != realWant {
		t.Fatalf("dir = %q; want %q", dir, realWant)
	}
}

func TestCmdInit_NoArgsNonTerminalRefuses(t *testing.T) {
	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	dir := t.TempDir()
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := cmdInitWithOptions(nil, "", "", &stdout, &stderr, true)

	if code == 0 {
		t.Fatalf("cmdInitWithOptions code = 0; want non-zero. stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("stderr = %q; want it to mention the non-interactive-terminal reason", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "city.toml")); !os.IsNotExist(err) {
		t.Fatalf("city.toml was created at cwd %s despite the guard; stat err = %v", dir, err)
	}
}

func TestCmdInit_ExplicitPathNonTerminalStillWorks(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	disableBootstrapForTests(t)

	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	target := filepath.Join(t.TempDir(), "bright-lights")
	var stdout, stderr bytes.Buffer
	code := cmdInitWithOptions([]string{target}, "codex", "", &stdout, &stderr, true)

	if code != 0 {
		t.Fatalf("cmdInitWithOptions code = %d, want 0. stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(target, "city.toml")); err != nil {
		t.Fatalf("city.toml not created at explicit path %s: %v", target, err)
	}
}

func TestCmdInit_NoArgsTerminalUnchanged(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	disableBootstrapForTests(t)

	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
		return true, 0
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	dir := t.TempDir()
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := cmdInitWithOptions(nil, "codex", "", &stdout, &stderr, true)

	if code != 0 {
		t.Fatalf("cmdInitWithOptions code = %d, want 0. stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "city.toml")); err != nil {
		t.Fatalf("city.toml not created at cwd %s: %v", dir, err)
	}
}

func TestCmdInitFromFile_NoArgsNonTerminalRefuses(t *testing.T) {
	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	code := cmdInitFromFileWithOptionsInternal("nonexistent.toml", nil, "", &stdout, &stderr, true, false, false)

	if code == 0 {
		t.Fatalf("cmdInitFromFileWithOptionsInternal code = 0; want non-zero. stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("stderr = %q; want it to mention the non-interactive-terminal reason", stderr.String())
	}
}

func TestCmdInitFromDir_NoArgsNonTerminalRefuses(t *testing.T) {
	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	t.Chdir(t.TempDir())
	srcDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := cmdInitFromDirWithOptionsInternal(srcDir, nil, "", &stdout, &stderr, true, false)

	if code == 0 {
		t.Fatalf("cmdInitFromDirWithOptionsInternal code = 0; want non-zero. stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "interactive terminal") {
		t.Fatalf("stderr = %q; want it to mention the non-interactive-terminal reason", stderr.String())
	}
}

// TestResolveStartDir_NoArgsNonTerminalUsesCWD pins the scope boundary of the
// implicit-cwd guard: it applies to the state-creating gc init entry points,
// not to start/restart. A no-path start under non-interactive stdin must still
// resolve cwd, because requireBootstrappedCity rejects a cwd that is not
// inside an existing city before any side effect runs — there is no state to
// leak, and guarding here breaks the documented scripted flow (README
// quickstart, gc start --foreground, and the 01-hello-gas-city testscript).
func TestResolveStartDir_NoArgsNonTerminalUsesCWD(t *testing.T) {
	oldCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = oldCityFlag })

	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	t.Chdir(t.TempDir())
	want, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	dir, err := resolveStartDir(nil)
	if err != nil {
		t.Fatalf("resolveStartDir(nil) error = %v; want nil (the guard must not cover start)", err)
	}
	if dir != want {
		t.Fatalf("dir = %q; want %q", dir, want)
	}
}

func TestResolveStartDir_NoArgsTerminalUnchanged(t *testing.T) {
	oldCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = oldCityFlag })

	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return true }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	t.Chdir(t.TempDir())
	realWant, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	dir, err := resolveStartDir(nil)
	if err != nil {
		t.Fatalf("resolveStartDir(nil) error = %v; want nil", err)
	}
	if dir != realWant {
		t.Fatalf("dir = %q; want %q", dir, realWant)
	}
}

func TestResolveStartDir_ExplicitArgNonTerminalStillWorks(t *testing.T) {
	oldCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = oldCityFlag })

	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	target := t.TempDir()
	dir, err := resolveStartDir([]string{target})
	if err != nil {
		t.Fatalf("resolveStartDir([]string{%q}) error = %v; want nil", target, err)
	}
	if dir != target {
		t.Fatalf("dir = %q; want %q", dir, target)
	}
}

func TestResolveStartDir_CityFlagNonTerminalStillWorks(t *testing.T) {
	target := t.TempDir()
	oldCityFlag := cityFlag
	cityFlag = target
	t.Cleanup(func() { cityFlag = oldCityFlag })

	old := stdinIsRealTerminal
	stdinIsRealTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsRealTerminal = old })

	dir, err := resolveStartDir(nil)
	if err != nil {
		t.Fatalf("resolveStartDir(nil) error = %v; want nil", err)
	}
	if dir != target {
		t.Fatalf("dir = %q; want %q", dir, target)
	}
}
