//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

// sourceWorkflowLockHeld reports whether the one source-workflow lock under
// cityPath is held right now. A non-blocking flock through a fresh descriptor
// of its lock file is refused while any other descriptor holds it, in this
// process or another: the exclusion the lock gives overlapping cooks.
func sourceWorkflowLockHeld(t *testing.T, cityPath string) bool {
	t.Helper()
	names := sourceWorkflowLockFiles(t, cityPath)
	if len(names) != 1 {
		t.Fatalf("source-workflow lock files under %s = %v, want exactly the one the cook takes", cityPath, names)
	}
	f, err := os.OpenFile(filepath.Join(citylayout.RuntimeDataDir(cityPath), "sling-source-locks", names[0]), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening source-workflow lock %s: %v", names[0], err)
	}
	defer f.Close() //nolint:errcheck // test cleanup
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return true
	}
	if err != nil {
		t.Fatalf("probing source-workflow lock %s: %v", names[0], err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("releasing the probe's hold on source-workflow lock %s: %v", names[0], err)
	}
	return false
}
