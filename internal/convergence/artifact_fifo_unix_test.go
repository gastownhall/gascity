//go:build unix

package convergence

// A FIFO is a Unix file type. Windows named pipes live in a separate object
// namespace rather than the file system, so no directory walk can encounter
// one and there is no Windows counterpart to this case.

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
