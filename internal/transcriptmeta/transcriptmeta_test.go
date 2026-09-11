package transcriptmeta

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withEnabled flips the process gate on for the duration of a test and restores
// it afterward, so the global never leaks across tests.
func withEnabled(t *testing.T) {
	t.Helper()
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(false) })
}

func writeTranscript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte("transcript body\n"), 0o644); err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
	return path
}

func TestWrite_DisabledIsNoOp(t *testing.T) {
	// Gate defaults to off — the package-level default must never write.
	dir := t.TempDir()
	path := writeTranscript(t, dir)

	ok, err := Write(path, "gc-session-1")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if ok {
		t.Fatal("Write reported ok=true while disabled")
	}
	if _, err := os.Stat(path + Suffix); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar when disabled, stat err = %v", err)
	}
}

func TestWrite_EnabledWritesSidecar(t *testing.T) {
	withEnabled(t)
	dir := t.TempDir()
	path := writeTranscript(t, dir)

	ok, err := Write(path, "gc-session-1")
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !ok {
		t.Fatal("Write reported ok=false for an existing transcript")
	}
	got, err := os.ReadFile(path + Suffix)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(got) != "gc-session-1\n" {
		t.Fatalf("sidecar content = %q, want %q", got, "gc-session-1\n")
	}
	info, err := os.Stat(path + Suffix)
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("sidecar perm = %o, want 600", perm)
	}
}

func TestWrite_BlankArgsAreNoOps(t *testing.T) {
	withEnabled(t)
	dir := t.TempDir()
	path := writeTranscript(t, dir)

	if ok, err := Write("", "gc-session-1"); err != nil || ok {
		t.Fatalf("Write(blank path): ok=%v err=%v", ok, err)
	}
	if ok, err := Write(path, "   "); err != nil || ok {
		t.Fatalf("Write(blank id): ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(path + Suffix); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar for blank args, stat err = %v", err)
	}
}

func TestWrite_MissingTranscriptIsNoOp(t *testing.T) {
	withEnabled(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-written-yet.jsonl")

	// No transcript on disk yet: EvalSymlinks fails, so ok=false invites the
	// caller to retry. Nothing is written and the call does not error.
	ok, err := Write(missing, "gc-session-1")
	if err != nil {
		t.Fatalf("Write(missing): %v", err)
	}
	if ok {
		t.Fatal("Write reported ok=true for a missing transcript")
	}
	if _, err := os.Stat(missing + Suffix); !os.IsNotExist(err) {
		t.Fatalf("expected no sidecar for missing transcript, stat err = %v", err)
	}
}

// TestWrite_ResolvesSymlinkDir is the gate-#1 retirement check: gc writes the
// sidecar at the symlink-resolved transcript path, which is exactly the path an
// out-of-band reader computes via filepath.EvalSymlinks. An ancestor symlink
// must not split the two locations.
func TestWrite_ResolvesSymlinkDir(t *testing.T) {
	withEnabled(t)
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	path := writeTranscript(t, realDir)

	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	ok, err := Write(filepath.Join(linkDir, "session.jsonl"), "gc-session-1")
	if err != nil || !ok {
		t.Fatalf("Write(via symlink): ok=%v err=%v", ok, err)
	}

	// Sidecar lands beside the REAL transcript, regardless of the symlinked
	// path the caller passed.
	resolvedSidecar := path + Suffix
	got, err := os.ReadFile(resolvedSidecar)
	if err != nil {
		t.Fatalf("read resolved sidecar %q: %v", resolvedSidecar, err)
	}
	if string(got) != "gc-session-1\n" {
		t.Fatalf("resolved sidecar content = %q, want %q", got, "gc-session-1\n")
	}
}

// TestWrite_ResolveFaultSurfacesError proves a symlink-resolution failure that
// is NOT the expected "transcript not written yet" case (here a self-referential
// symlink, which yields ELOOP) is returned as a non-nil error rather than
// swallowed as a quiet (false, nil) retry, so a persistent filesystem fault
// becomes diagnosable via the caller's debug log.
func TestWrite_ResolveFaultSurfacesError(t *testing.T) {
	withEnabled(t)
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop.jsonl")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatalf("seed symlink loop: %v", err)
	}

	ok, err := Write(loop, "gc-session-1")
	if err == nil {
		t.Fatal("Write must surface a non-ENOENT EvalSymlinks fault as a non-nil error")
	}
	if ok {
		t.Fatal("Write must report ok=false on a resolution fault")
	}
	if _, statErr := os.Stat(loop + Suffix); !os.IsNotExist(statErr) {
		t.Fatalf("expected no sidecar on resolution fault, stat err = %v", statErr)
	}
}

func TestWrite_IdempotentDoesNotChurn(t *testing.T) {
	withEnabled(t)
	dir := t.TempDir()
	path := writeTranscript(t, dir)

	if ok, err := Write(path, "gc-session-1"); err != nil || !ok {
		t.Fatalf("first Write: ok=%v err=%v", ok, err)
	}

	// Age the mtime so an unintended rewrite would be observable.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path+Suffix, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if ok, err := Write(path, "gc-session-1"); err != nil || !ok {
		t.Fatalf("second Write: ok=%v err=%v", ok, err)
	}
	second, err := os.Stat(path + Suffix)
	if err != nil {
		t.Fatalf("stat after second write: %v", err)
	}
	if !second.ModTime().Equal(old) {
		t.Fatalf("idempotent write churned the file: mtime moved from %v to %v", old, second.ModTime())
	}
}

func TestWrite_UpdatesOnChange(t *testing.T) {
	withEnabled(t)
	dir := t.TempDir()
	path := writeTranscript(t, dir)

	if ok, err := Write(path, "gc-session-1"); err != nil || !ok {
		t.Fatalf("first Write: ok=%v err=%v", ok, err)
	}
	if ok, err := Write(path, "gc-session-2"); err != nil || !ok {
		t.Fatalf("second Write: ok=%v err=%v", ok, err)
	}
	got, err := os.ReadFile(path + Suffix)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if string(got) != "gc-session-2\n" {
		t.Fatalf("sidecar content = %q, want updated %q", got, "gc-session-2\n")
	}
}

func TestEnabledToggle(t *testing.T) {
	SetEnabled(false)
	if Enabled() {
		t.Fatal("Enabled() true after SetEnabled(false)")
	}
	SetEnabled(true)
	t.Cleanup(func() { SetEnabled(false) })
	if !Enabled() {
		t.Fatal("Enabled() false after SetEnabled(true)")
	}
}
