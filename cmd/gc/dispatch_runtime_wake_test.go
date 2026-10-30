package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

// TestWriteDispatchWakeFileAdvancesMtime pins that every wake is observable:
// workers detect a wake by the file's mtime changing, so a second wake on an
// existing file must move its mtime forward (ga-vnycm2.35: opening the file
// without writing left the mtime at creation time, so only the first wake was
// ever seen).
func TestWriteDispatchWakeFileAdvancesMtime(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDispatchWakeFile(dir)
	path := dispatchWakeFile(dir)

	// Age the first wake by an hour so the second is distinguishable even by a
	// reader that compares whole-second mtimes (stat -c %Y).
	firstWake := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, firstWake, firstWake); err != nil {
		t.Fatal(err)
	}

	before := time.Now().Add(-time.Second)
	writeDispatchWakeFile(dir)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("dispatch-wake file missing after second wake: %v", err)
	}
	if got := info.ModTime(); !got.After(before) {
		t.Fatalf("second wake left mtime at %v (first wake %v), want after %v: waiting workers cannot observe it", got, firstWake, before)
	}
}

func TestWriteDispatchWakeFileNoopOnMissingCityPath(_ *testing.T) {
	// .gc/ does not exist; write must be best-effort (no panic, no error return).
	writeDispatchWakeFile("/nonexistent/city/path/that/does/not/exist")
}
