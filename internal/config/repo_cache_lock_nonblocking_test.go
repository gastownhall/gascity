//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// holdRepoCacheLock takes mode on the cache root's lock file and releases it
// when the test ends. It returns the root so callers can pass it straight to
// the helper under test.
func holdRepoCacheLock(t *testing.T, mode int) string {
	t.Helper()
	root := t.TempDir()
	lockFile, err := os.OpenFile(filepath.Join(root, repoCacheLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock file: %v", err)
	}
	t.Cleanup(func() { lockFile.Close() }) //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), mode); err != nil {
		t.Fatalf("Flock(%d): %v", mode, err)
	}
	t.Cleanup(func() { syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }) //nolint:errcheck
	return root
}

func TestTryWithRepoCacheReadLockReportsBusyWithoutWaiting(t *testing.T) {
	root := holdRepoCacheLock(t, syscall.LOCK_EX)

	called := false
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- TryWithRepoCacheReadLock(root, func() error {
			called = true
			return nil
		})
	}()

	// A blocking acquisition never returns while the exclusive lock is held,
	// so this select is what fails the test if LOCK_NB is ever dropped.
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("TryWithRepoCacheReadLock blocked on a held exclusive lock")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("returned after %v, want an immediate refusal", elapsed)
	}
	if !errors.Is(err, ErrRepoCacheBusy) {
		t.Fatalf("err = %v, want ErrRepoCacheBusy", err)
	}
	if called {
		t.Fatal("callback ran while the exclusive lock was held")
	}
}

func TestTryWithRepoCacheReadLockRunsWhenLockIsFree(t *testing.T) {
	root := t.TempDir()
	called := false
	if err := TryWithRepoCacheReadLock(root, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("TryWithRepoCacheReadLock: %v", err)
	}
	if !called {
		t.Fatal("callback did not run with the lock free")
	}
}

// A shared holder must not lock out a shared try-acquisition. This is the
// control that distinguishes a correct LOCK_SH|LOCK_NB from a LOCK_EX|LOCK_NB
// that would refuse every concurrent reader and quietly disable pack
// discovery whenever any other gc process is reading the cache.
func TestTryWithRepoCacheReadLockSharesWithOtherReaders(t *testing.T) {
	root := holdRepoCacheLock(t, syscall.LOCK_SH)

	called := false
	if err := TryWithRepoCacheReadLock(root, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("TryWithRepoCacheReadLock: %v", err)
	}
	if !called {
		t.Fatal("callback did not run alongside another shared reader")
	}
}

// Missing root means there is no cache to contend for, so the callback runs
// and no lock file is created — matching WithRepoCacheReadLock.
func TestTryWithRepoCacheReadLockDoesNotCreateMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	called := false
	if err := TryWithRepoCacheReadLock(root, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("TryWithRepoCacheReadLock: %v", err)
	}
	if !called {
		t.Fatal("callback did not run for a missing cache root")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root stat err = %v, want not exist", err)
	}
}

// The callback's own error must reach the caller unchanged, so a busy cache
// stays distinguishable from work that ran and failed.
func TestTryWithRepoCacheReadLockPropagatesCallbackError(t *testing.T) {
	root := t.TempDir()
	sentinel := errors.New("callback failed")
	err := TryWithRepoCacheReadLock(root, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the callback error", err)
	}
	if errors.Is(err, ErrRepoCacheBusy) {
		t.Fatal("callback error was reported as a busy cache")
	}
}
