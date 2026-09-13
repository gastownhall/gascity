package convergence

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestEnsureArtifactDir_Creates(t *testing.T) {
	fake := fsys.NewFake()
	dir, err := EnsureArtifactDir(fake, "/city", "bead-1", 2)
	if err != nil {
		t.Fatalf("EnsureArtifactDir: %v", err)
	}

	want := filepath.Join("/city", ".gc", "artifacts", "bead-1", "iter-2")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}

	// Verify MkdirAll was called.
	if len(fake.Calls) != 1 {
		t.Fatalf("call count = %d, want 1", len(fake.Calls))
	}
	if fake.Calls[0].Method != "MkdirAll" {
		t.Errorf("method = %q, want MkdirAll", fake.Calls[0].Method)
	}
	if fake.Calls[0].Path != want {
		t.Errorf("path = %q, want %q", fake.Calls[0].Path, want)
	}
}

func TestEnsureArtifactDir_AlreadyExists(t *testing.T) {
	fake := fsys.NewFake()
	// Pre-populate the directory.
	dir := filepath.Join("/city", ".gc", "artifacts", "bead-1", "iter-1")
	fake.Dirs[dir] = true

	got, err := EnsureArtifactDir(fake, "/city", "bead-1", 1)
	if err != nil {
		t.Fatalf("EnsureArtifactDir: %v", err)
	}
	if got != dir {
		t.Errorf("dir = %q, want %q", got, dir)
	}
}

func TestEnsureArtifactDir_MkdirError(t *testing.T) {
	fake := fsys.NewFake()
	dir := filepath.Join("/city", ".gc", "artifacts", "bead-1", "iter-1")
	fake.Errors[dir] = os.ErrPermission

	_, err := EnsureArtifactDir(fake, "/city", "bead-1", 1)
	if err == nil {
		t.Fatal("expected error from MkdirAll failure")
	}
	if !strings.Contains(err.Error(), "creating artifact directory") {
		t.Errorf("error should have context, got: %v", err)
	}
}

func TestValidateArtifactDir_Clean(t *testing.T) {
	dir := t.TempDir()

	// Create a regular file.
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory with a file.
	subDir := filepath.Join(dir, "sub")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "nested.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ValidateArtifactDir(dir); err != nil {
		t.Errorf("expected no error for clean dir, got: %v", err)
	}
}

func TestValidateArtifactDir_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateArtifactDir(dir); err != nil {
		t.Errorf("expected no error for empty dir, got: %v", err)
	}
}

func TestValidateArtifactDir_SymlinkOutside(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a symlink pointing outside the artifact directory.
	symlink := filepath.Join(dir, "escape")
	if err := os.Symlink(outsideDir, symlink); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	err := ValidateArtifactDir(dir)
	if err == nil {
		t.Fatal("expected error for symlink pointing outside")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "outside") {
		t.Errorf("error should mention symlink/outside, got: %v", err)
	}
}

func TestValidateArtifactDir_SymlinkInside(t *testing.T) {
	dir := t.TempDir()

	// Create a file and a symlink pointing to it within the directory.
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	if err := ValidateArtifactDir(dir); err != nil {
		t.Errorf("expected no error for internal symlink, got: %v", err)
	}
}

func TestValidateArtifactDir_FIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")

	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo not available: %v", err)
	}

	err := ValidateArtifactDir(dir)
	if err == nil {
		t.Fatal("expected error for FIFO in artifact directory")
	}
	if !strings.Contains(err.Error(), "unsafe file type") {
		t.Errorf("error should mention unsafe file type, got: %v", err)
	}
}

// Regression-pins ValidateArtifactDir's existence-check behavior (refs
// ga-iawy13.4): a missing artifact directory must still produce an error.
// This root EvalSymlinks site is deliberate existence checking, not
// comparison preparation, and must keep failing the same way after the
// canonical-path-at-ingest migration.
func TestValidateArtifactDir_MissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	err := ValidateArtifactDir(dir)
	if err == nil {
		t.Fatal("expected error for missing artifact directory, got nil")
	}
}
