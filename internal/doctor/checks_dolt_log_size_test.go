package doctor

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestDoltLogSizeCheck_Skipped(t *testing.T) {
	c := NewDoltLogSizeCheck(t.TempDir(), true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want skipped", r.Message)
	}
}

// TestDoltLogSizeCheck_NotApplicable covers the managed-Dolt gate: a file
// backend or external endpoint has no dolt.log for this check to measure,
// and must not be reported on. The gate is set directly rather than through
// a city fixture so the assertion is about the gate and nothing else.
func TestDoltLogSizeCheck_NotApplicable(t *testing.T) {
	c := &DoltLogSizeCheck{cityPath: t.TempDir(), applicableKnown: true, applicable: false}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want skipped", r.Message)
	}
}

func TestDoltLogSizeCheck_MissingLog(t *testing.T) {
	dir := setupManagedDoltCity(t)
	c := NewDoltLogSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not present") {
		t.Errorf("message = %q, want not-present text", r.Message)
	}
}

// TestDoltLogSizeCheck_DefaultPath exercises the path resolver rather than
// the GC_DOLT_LOG_FILE override, so a layout change cannot silently make
// every other case in this file measure a file the server never writes.
func TestDoltLogSizeCheck_DefaultPath(t *testing.T) {
	dir := setupManagedDoltCity(t)
	logPath := filepath.Join(citylayout.RuntimeDataDir(dir), "packs", "dolt", "dolt.log")
	writeFakeFile(t, logPath, 4*1024*1024) // 4 MB

	c := NewDoltLogSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "4.0 MB") {
		t.Errorf("message = %q, want the measured size", r.Message)
	}
}

func TestDoltLogSizeCheck_WarnAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	// 1 GB — above warn (512 MB), below error (2 GB). Sparse via Truncate.
	writeFakeFile(t, logPath, 1024*1024*1024)
	t.Setenv("GC_DOLT_LOG_FILE", logPath)

	c := NewDoltLogSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "approaching threshold") {
		t.Errorf("message = %q, want approaching-threshold text", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint is empty, want truncation guidance")
	}
}

func TestDoltLogSizeCheck_ErrorAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	writeFakeFile(t, logPath, 3*1024*1024*1024) // 3 GB, above error (2 GB)
	t.Setenv("GC_DOLT_LOG_FILE", logPath)

	c := NewDoltLogSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "nothing rotates it") {
		t.Errorf("message = %q, want rotation text", r.Message)
	}
}

func TestDoltLogSizeCheck_EnvThresholdOverride(t *testing.T) {
	dir := setupManagedDoltCity(t)
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	writeFakeFile(t, logPath, 1024) // 1 KB — far below every default
	t.Setenv("GC_DOLT_LOG_FILE", logPath)
	t.Setenv("GC_DOLT_LOG_WARN_BYTES", "512")
	t.Setenv("GC_DOLT_LOG_ERROR_BYTES", "1000000")

	c := NewDoltLogSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning under lowered threshold; msg = %s", r.Status, r.Message)
	}
}

// TestDoltLogSizeCheck_InvalidEnvThreshold guards the fallback: a typo in
// the env var must not silently disable the canary.
func TestDoltLogSizeCheck_InvalidEnvThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	logPath := filepath.Join(t.TempDir(), "dolt.log")
	writeFakeFile(t, logPath, 1024*1024) // 1 MB
	t.Setenv("GC_DOLT_LOG_FILE", logPath)
	t.Setenv("GC_DOLT_LOG_WARN_BYTES", "not-a-number")

	c := NewDoltLogSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (default 512 MB threshold restored); msg = %s", r.Status, r.Message)
	}
}

func TestDoltLogSizeCheck_NotAFile(t *testing.T) {
	dir := setupManagedDoltCity(t)
	logPath := t.TempDir() // a directory where the log should be
	t.Setenv("GC_DOLT_LOG_FILE", logPath)

	c := NewDoltLogSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not a regular file") {
		t.Errorf("message = %q, want not-a-regular-file text", r.Message)
	}
}

func TestDoltLogSizeCheck_CannotFix(t *testing.T) {
	c := NewDoltLogSizeCheck(t.TempDir(), false)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Errorf("Fix() = %v, want nil", err)
	}
}
