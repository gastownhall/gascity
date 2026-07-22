package beads

import (
	"errors"
	"fmt"
	"io"
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
	if fl.f != nil {
		return errors.New("flock lock: already locked")
	}
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

// TryLock attempts to acquire an exclusive flock without blocking. The
// returned boolean is false only when another process currently owns the
// lock; other failures are returned as errors.
func (fl *FileFlock) TryLock() (bool, error) {
	if fl.f != nil {
		return false, errors.New("flock try-lock: already locked")
	}
	f, err := os.OpenFile(fl.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, fmt.Errorf("flock open: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return false, fmt.Errorf("flock try-lock: %w", err)
	}
	fl.f = f
	return true, nil
}

// WriteLocked replaces the contents of the already-locked file without
// replacing its inode. Callers use this for owner records that must describe
// the holder of this exact flock.
func (fl *FileFlock) WriteLocked(data []byte) error {
	if fl.f == nil {
		return errors.New("flock write: not locked")
	}
	if err := fl.f.Truncate(0); err != nil {
		return fmt.Errorf("flock truncate: %w", err)
	}
	if _, err := fl.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("flock seek: %w", err)
	}
	if len(data) > 0 {
		if _, err := fl.f.Write(data); err != nil {
			return fmt.Errorf("flock write: %w", err)
		}
	}
	if err := fl.f.Sync(); err != nil {
		return fmt.Errorf("flock sync: %w", err)
	}
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
