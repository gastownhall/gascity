package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/platform/filelock"
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
	lock *filelock.Lock
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
	lock, err := filelock.Acquire(f, filelock.Options{Mode: filelock.Exclusive, Label: fl.path})
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("flock lock: %w", err)
	}
	fl.f = f
	fl.lock = lock
	return nil
}

// Unlock releases the flock and closes the lock file.
func (fl *FileFlock) Unlock() error {
	if fl.f == nil {
		return nil
	}
	// Unlock then close; ignore unlock error if close succeeds.
	if fl.lock != nil {
		_ = fl.lock.Release()
	}
	err := fl.f.Close()
	fl.f = nil
	fl.lock = nil
	return err
}

// nopLocker is a no-op Locker for use when file locking is not needed
// (e.g., tests with in-memory filesystems).
type nopLocker struct{}

func (nopLocker) Lock() error   { return nil }
func (nopLocker) Unlock() error { return nil }

// flockPollInterval is how long a bounded flock wait sleeps between attempts
// while the lock is contended.
const flockPollInterval = 10 * time.Millisecond

// flockAcquire is the single call every bounded flock attempt goes through. It
// is a variable so a test can count attempts and inject errnos.
var flockAcquire = filelock.Acquire

// flockExclusive takes an exclusive lock on f, waiting for contention only as
// long as ctx allows.
func flockExclusive(_ context.Context, _ *os.File, _ string) (*filelock.Lock, error) {
	return nil, errors.New("flockExclusive: not implemented")
}

// tryFlockExclusive makes exactly one non-blocking attempt at an exclusive lock
// on f and reports contention as filelock.ErrBusy.
func tryFlockExclusive(_ *os.File, _ string) (*filelock.Lock, error) {
	return nil, errors.New("tryFlockExclusive: not implemented")
}

// mapStoreTimeout turns the end of a store-owned wait into ErrStoreTimeout.
func mapStoreTimeout(_, _ context.Context, err error) error {
	return err
}
