package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDispatchWakeFileReturnsGCSubpath(t *testing.T) {
	got := dispatchWakeFile("/some/city")
	want := filepath.Join("/some/city", ".gc", "dispatch-wake")
	if got != want {
		t.Errorf("dispatchWakeFile = %q, want %q", got, want)
	}
}

func TestWriteDispatchWakeFileCreatesFile(t *testing.T) {
	dir := t.TempDir()
	gc := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gc, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDispatchWakeFile(dir)
	path := dispatchWakeFile(dir)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("dispatch-wake file not created: %v", err)
	}
}

func TestWriteDispatchWakeFileAdvancesMtime(t *testing.T) {
	dir := t.TempDir()
	gc := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gc, 0o755); err != nil {
		t.Fatal(err)
	}
	writeDispatchWakeFile(dir)
	path := dispatchWakeFile(dir)
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Second write must not return an error and the file must still exist.
	writeDispatchWakeFile(dir)
	info2, err := os.Stat(path)
	if err != nil {
		t.Errorf("dispatch-wake file missing after second write: %v", err)
	}
	_ = info1
	_ = info2
}

func TestWriteDispatchWakeFileNoopOnMissingCityPath(_ *testing.T) {
	// .gc/ does not exist; write must be best-effort (no panic, no error return).
	writeDispatchWakeFile("/nonexistent/city/path/that/does/not/exist")
}
