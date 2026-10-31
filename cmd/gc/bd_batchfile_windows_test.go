//go:build windows

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

// TestBdBatchFileCheckWarnsBeforeWritesFail pins that gc says up front when bd
// resolves to a batch file. gc refuses to hand such a bd any text cmd.exe would
// act on, so without the warning the first sign is a failed multi-line write.
func TestBdBatchFileCheckWarnsBeforeWritesFail(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "bd.cmd")
	native := filepath.Join(dir, "bd.exe")
	for _, path := range []string{shim, native} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, path := range []string{shim, shim + ".", filepath.Join(dir, "BD.BAT")} {
		r := (&bdBatchFileCheck{resolve: func() (string, error) { return path, nil }}).Run(&doctor.CheckContext{})
		if r.Status != doctor.StatusWarning {
			t.Fatalf("bd at %s: status = %v (%s), want a warning", path, r.Status, r.Message)
		}
		for _, want := range []string{path, "batch file", "line break"} {
			if !strings.Contains(r.Message, want) {
				t.Errorf("bd at %s: message %q lacks %q", path, r.Message, want)
			}
		}
		if !strings.Contains(r.FixHint, "BD_BIN") {
			t.Errorf("bd at %s: fix hint %q does not name BD_BIN", path, r.FixHint)
		}
	}

	r := (&bdBatchFileCheck{resolve: func() (string, error) { return native, nil }}).Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK {
		t.Errorf("bd.exe: status = %v (%s), want OK", r.Status, r.Message)
	}
	r = (&bdBatchFileCheck{resolve: func() (string, error) { return "", errBdNotOnPath }}).Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK {
		t.Errorf("bd not found: status = %v (%s), want OK: the missing binary is another check's finding", r.Status, r.Message)
	}

	if got := bdBatchFileWarning(native); got != "" {
		t.Errorf("bdBatchFileWarning(bd.exe) = %q, want none", got)
	}
	if got := bdBatchFileWarning(shim); !strings.Contains(got, shim) || !strings.Contains(got, "BD_BIN") {
		t.Errorf("bdBatchFileWarning(bd.cmd) = %q, want the path and the remedy", got)
	}
}

// TestDefaultStartWarnsOnceWhenBdIsABatchFile pins that the default gc start,
// which hands the city to the supervisor, says up front that bd is a batch
// file: once, on stderr (so --json keeps stdout to its one record). A plain gc
// start prints it with the other start warnings and counts it in its summary
// line; gc start --json and gc restart print it before the city is registered.
func TestDefaultStartWarnsOnceWhenBdIsABatchFile(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	stubInitDependencyChecks(t)
	oldAlive := supervisorAliveHook
	supervisorAliveHook = func() int { return 0 }
	t.Cleanup(func() { supervisorAliveHook = oldAlive })
	oldDry, oldNoAR := dryRunMode, noAutoRestartMode
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() { dryRunMode, noAutoRestartMode = oldDry, oldNoAR })

	shim := filepath.Join(t.TempDir(), "bd.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(t.TempDir(), "bd-city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"bd-city\"\n\n[workspace.env]\nBD_BIN = " + strconv.Quote(shim) + "\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	entryPoints := []struct {
		name string
		// summary marks the human gc start, whose output proxy holds warning
		// lines until it flushes them ahead of its summary line.
		summary bool
		start   func(stdout, stderr io.Writer) int
	}{
		{"gc start", true, func(stdout, stderr io.Writer) int {
			return doStart([]string{cityPath}, false, stdout, stderr)
		}},
		{"gc start --json", false, func(stdout, stderr io.Writer) int {
			return doStartJSON([]string{cityPath}, false, true, stdout, stderr)
		}},
		{"gc restart", false, func(stdout, stderr io.Writer) int {
			return doStartWithNameOverride([]string{cityPath}, false, stdout, stderr, "")
		}},
	}
	for _, entry := range entryPoints {
		var stdout, stderr bytes.Buffer
		warnedBeforeRegistration := false
		oldRegister := registerCityWithSupervisorTestHook
		registerCityWithSupervisorTestHook = func(_ string, _ string, _ io.Writer, _ io.Writer) (bool, int) {
			warnedBeforeRegistration = strings.Contains(stderr.String(), "batch file")
			return true, 0
		}
		code := entry.start(&stdout, &stderr)
		registerCityWithSupervisorTestHook = oldRegister
		if code != 0 {
			t.Fatalf("%s: exit %d; stderr=%q", entry.name, code, stderr.String())
		}
		if n := strings.Count(stderr.String(), "batch file"); n != 1 {
			t.Errorf("%s: stderr names the batch file %d times, want once:\n%s", entry.name, n, stderr.String())
		}
		for _, want := range []string{"gc start: warning:", shim, "BD_BIN"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("%s: stderr lacks %q:\n%s", entry.name, want, stderr.String())
			}
		}
		if strings.Contains(stdout.String(), "batch file") {
			t.Errorf("%s: the warning reached stdout:\n%s", entry.name, stdout.String())
		}
		if !entry.summary {
			if !warnedBeforeRegistration {
				t.Errorf("%s: the warning was not on stderr before the city was registered", entry.name)
			}
			continue
		}
		warning := strings.Index(stderr.String(), "gc start: warning:")
		summary := strings.Index(stderr.String(), "gc-start: ")
		if warning < 0 || summary < warning {
			t.Errorf("%s: the warning is not printed ahead of the summary line:\n%s", entry.name, stderr.String())
		}
		if !strings.Contains(stderr.String(), " warnings=1 ") {
			t.Errorf("%s: the summary line does not count the warning once:\n%s", entry.name, stderr.String())
		}
	}
}
