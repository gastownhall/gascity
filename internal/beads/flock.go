package beads

import (
	"fmt"
	"os"
	"syscall"
)

// Locker abstracts file-level locking for cross-process synchronization.
// FileStore uses it to serialize concurrent writers (CLI + controller).
type Locker interface {
	// Lock acquires an exclusive lock, blocking until available.
	Lock() error
	// Unlock releases the lock.
	Unlock() error
}

// FileFlock implements Locker using flock(2) on the given path.
// The lock file is created if it does not exist.
type FileFlock struct {
	path string
	f    *os.File
}

// NewFileFlock returns a new FileFlock that locks the given path.
func NewFileFlock(path string) *FileFlock {
	return &FileFlock{path: path}
}

// Lock acquires an exclusive flock, creating the lock file if needed.
func (fl *FileFlock) Lock() error {
	f, err := os.OpenFile(fl.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("flock open: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return fmt.Errorf("flock lock: %w", err)
	}
	fl.f = f
	return nil
}

// Unlock releases the flock and closes the lock file.
func (fl *FileFlock) Unlock() error {
	if fl.f == nil {
		return nil
	}
	// Unlock then close; ignore unlock error if close succeeds.
	syscall.Flock(int(fl.f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock before close
	err := fl.f.Close()
	fl.f = nil
	return err
}

// nopLocker is a no-op Locker for use when file locking is not needed
// (e.g., tests with in-memory filesystems).
type nopLocker struct{}

func (nopLocker) Lock() error   { return nil }
func (nopLocker) Unlock() error { return nil }
