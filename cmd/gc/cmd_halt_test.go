package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// writeHaltTestCityToml writes a minimal city.toml so resolveCommandCity
// can validate the city path. Tests that only exercise file helpers
// (writeHaltFile / removeHaltFile / isCityHalted) do not need this.
func writeHaltTestCityToml(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"halt-test\"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
}

func TestWriteHaltFile_CreatesFlag(t *testing.T) {
	city := t.TempDir()
	if isCityHalted(city) {
		t.Fatalf("expected not halted initially")
	}
	if err := writeHaltFile(city); err != nil {
		t.Fatalf("writeHaltFile: %v", err)
	}
	if !isCityHalted(city) {
		t.Fatalf("expected halted after writeHaltFile")
	}

	// Verify the file lives under the canonical runtime dir.
	want := filepath.Join(citylayout.RuntimeDataDir(city), "halt")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("halt file not at %s: %v", want, err)
	}
}

func TestWriteHaltFile_Idempotent(t *testing.T) {
	city := t.TempDir()
	if err := writeHaltFile(city); err != nil {
		t.Fatalf("writeHaltFile #1: %v", err)
	}
	// Stamp the file with a distinctive payload so we can detect an
	// accidental overwrite on the second call.
	path := haltFilePath(city)
	stamp := []byte("original-stamp")
	if err := os.WriteFile(path, stamp, 0o644); err != nil {
		t.Fatalf("overwrite halt file: %v", err)
	}
	if err := writeHaltFile(city); err != nil {
		t.Fatalf("writeHaltFile #2: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read halt file: %v", err)
	}
	if string(got) != string(stamp) {
		t.Fatalf("halt file was overwritten by idempotent call: got %q, want %q", got, stamp)
	}
	if !isCityHalted(city) {
		t.Fatalf("expected halted after two writeHaltFile calls")
	}
}

func TestRemoveHaltFile_RemovesFlag(t *testing.T) {
	city := t.TempDir()
	if err := writeHaltFile(city); err != nil {
		t.Fatalf("writeHaltFile: %v", err)
	}
	if !isCityHalted(city) {
		t.Fatalf("expected halted before removal")
	}
	if err := removeHaltFile(city); err != nil {
		t.Fatalf("removeHaltFile: %v", err)
	}
	if isCityHalted(city) {
		t.Fatalf("expected not halted after removeHaltFile")
	}
}

func TestRemoveHaltFile_NoFileIsNoop(t *testing.T) {
	city := t.TempDir()
	// Runtime dir does not even exist yet; must still be a no-op.
	if err := removeHaltFile(city); err != nil {
		t.Fatalf("removeHaltFile on absent file: %v", err)
	}
	// And again, after the dir exists but the file does not.
	if err := os.MkdirAll(citylayout.RuntimeDataDir(city), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	if err := removeHaltFile(city); err != nil {
		t.Fatalf("removeHaltFile on absent file (dir present): %v", err)
	}
	if isCityHalted(city) {
		t.Fatalf("expected not halted")
	}
}

func TestCmdHalt_CreatesFlagViaCLI(t *testing.T) {
	city := t.TempDir()
	writeHaltTestCityToml(t, city)
	var stdout, stderr bytes.Buffer
	if rc := cmdHalt([]string{city}, &stdout, &stderr); rc != 0 {
		t.Fatalf("cmdHalt rc=%d stderr=%s", rc, stderr.String())
	}
	if !isCityHalted(city) {
		t.Fatalf("expected halted after cmdHalt")
	}
	if !strings.Contains(stdout.String(), "City halted") {
		t.Fatalf("stdout missing confirmation: %q", stdout.String())
	}
}

func TestCmdHalt_Idempotent(t *testing.T) {
	city := t.TempDir()
	writeHaltTestCityToml(t, city)
	var stdout, stderr bytes.Buffer
	if rc := cmdHalt([]string{city}, &stdout, &stderr); rc != 0 {
		t.Fatalf("cmdHalt #1 rc=%d stderr=%s", rc, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if rc := cmdHalt([]string{city}, &stdout, &stderr); rc != 0 {
		t.Fatalf("cmdHalt #2 rc=%d stderr=%s", rc, stderr.String())
	}
	if !isCityHalted(city) {
		t.Fatalf("expected halted after two cmdHalt calls")
	}
}

func TestCmdResume_RemovesHaltFlag(t *testing.T) {
	city := t.TempDir()
	writeHaltTestCityToml(t, city)
	if err := writeHaltFile(city); err != nil {
		t.Fatalf("writeHaltFile: %v", err)
	}
	if !isCityHalted(city) {
		t.Fatalf("precondition: city should be halted")
	}
	var stdout, stderr bytes.Buffer
	// cmdResume may return non-zero on the suspend-flag half because
	// apiClient/config paths touch real runtime, but its halt-cleanup
	// half (which runs before anything else) must have fired.
	_ = cmdResume([]string{city}, &stdout, &stderr)
	if isCityHalted(city) {
		t.Fatalf("cmdResume did not remove halt flag; stderr=%s", stderr.String())
	}
}

func TestCmdResume_NoHaltFlagIsNoop(t *testing.T) {
	city := t.TempDir()
	writeHaltTestCityToml(t, city)
	var stdout, stderr bytes.Buffer
	_ = cmdResume([]string{city}, &stdout, &stderr)
	if isCityHalted(city) {
		t.Fatalf("cmdResume should not have created a halt flag")
	}
	// The stderr we capture should not contain a halt-file removal
	// error. Suspend-file errors are allowed; we only care about the
	// halt channel here.
	if strings.Contains(stderr.String(), "remove halt file") {
		t.Fatalf("unexpected halt-removal error: %s", stderr.String())
	}
}

// ---------------------------------------------------------------------------
// Supervisor tick-gate behavior
// ---------------------------------------------------------------------------

func TestHaltGate_ChecksSkipTickWhenFlagPresent(t *testing.T) {
	city := t.TempDir()
	var g haltGate
	var log bytes.Buffer
	if g.check(city, &log) {
		t.Fatalf("gate reported halted with no flag file")
	}

	if err := writeHaltFile(city); err != nil {
		t.Fatalf("writeHaltFile: %v", err)
	}
	if !g.check(city, &log) {
		t.Fatalf("gate did not report halted after writeHaltFile")
	}
}

func TestHaltGate_ChecksProceedWhenFlagAbsent(t *testing.T) {
	city := t.TempDir()
	var g haltGate
	var log bytes.Buffer
	for i := 0; i < 5; i++ {
		if g.check(city, &log) {
			t.Fatalf("iteration %d: gate reported halted with no flag file", i)
		}
	}
	if log.Len() != 0 {
		t.Fatalf("unexpected log output on running path: %q", log.String())
	}
}

func TestHaltGate_TransitionLogFiresOncePerStateChange(t *testing.T) {
	city := t.TempDir()
	var g haltGate
	var log bytes.Buffer

	// Running → halted transition: write flag and check many times.
	if err := writeHaltFile(city); err != nil {
		t.Fatalf("writeHaltFile: %v", err)
	}
	for i := 0; i < 10; i++ {
		if !g.check(city, &log) {
			t.Fatalf("iteration %d: expected halted", i)
		}
	}
	haltLines := strings.Count(log.String(), "supervisor: halted")
	if haltLines != 1 {
		t.Fatalf("halt log fired %d times, want 1; log=%q", haltLines, log.String())
	}
	if !strings.Contains(log.String(), filepath.Join(".gc", "runtime", "halt")) {
		t.Fatalf("halt log missing resume hint: %q", log.String())
	}

	// Halted → running transition: remove flag and check many times.
	if err := removeHaltFile(city); err != nil {
		t.Fatalf("removeHaltFile: %v", err)
	}
	for i := 0; i < 10; i++ {
		if g.check(city, &log) {
			t.Fatalf("iteration %d: expected running", i)
		}
	}
	resumeLines := strings.Count(log.String(), "supervisor: resumed")
	if resumeLines != 1 {
		t.Fatalf("resume log fired %d times, want 1; log=%q", resumeLines, log.String())
	}

	// Subsequent running-state ticks must not re-emit.
	before := log.Len()
	for i := 0; i < 5; i++ {
		_ = g.check(city, &log)
	}
	if log.Len() != before {
		t.Fatalf("running steady state emitted extra log output: %q", log.String()[before:])
	}
}
