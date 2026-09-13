//go:build !windows

package worktree

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestPathLockIsExclusive proves the lock actually excludes, using a
// non-blocking acquisition of the very file lockPath derives so the assertion
// is a fact about the lock rather than about goroutine scheduling. A test that
// raced a blocked goroutine against a held lock could pass without any locking
// at all, whenever the contender simply had not run yet.
func TestPathLockIsExclusive(t *testing.T) {
	repo, _ := initTestRepo(t)
	path := filepath.Join(t.TempDir(), "gc-locked")

	lockFile, err := lockFilePath(repo, path)
	if err != nil {
		t.Fatalf("lockFilePath: %v", err)
	}

	held, err := lockPath(repo, path)
	if err != nil {
		t.Fatalf("lockPath: %v", err)
	}
	if err := tryFlock(t, lockFile); err == nil {
		held.unlock()
		t.Fatal("a second acquisition succeeded while the lock was held")
	}

	held.unlock()
	if err := tryFlock(t, lockFile); err != nil {
		t.Fatalf("acquisition after release failed: %v", err)
	}
}

// tryFlock attempts a non-blocking exclusive lock on an already-derived lock
// file and releases it again on success.
func tryFlock(t *testing.T, lockFile string) error {
	t.Helper()
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("opening lock file %q: %v", lockFile, err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
