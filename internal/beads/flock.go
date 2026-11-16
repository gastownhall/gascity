package beads

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gastownhall/gascity/internal/platform/filelock"
)

// Locker abstracts file-level locking for cross-process synchronization.
// FileStore uses it to serialize concurrent writers (CLI + controller).
type Locker interface {
	// Lock acquires an exclusive lock. It waits until the lock is held or
	// ctx ends.
	Lock(ctx context.Context) error
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
func (fl *FileFlock) Lock(ctx context.Context) error {
	f, err := os.OpenFile(fl.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("flock open: %w", err)
	}
	lock, err := acquireFlock(ctx, f, fl.path)
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

func (nopLocker) Lock(context.Context) error { return nil }
func (nopLocker) Unlock() error              { return nil }

// flockPollInterval is how long acquireFlock waits between attempts on a
// contended lock.
const flockPollInterval = 10 * time.Millisecond

// flockAcquire is the kernel lock call, a seam for tests.
var flockAcquire = filelock.Acquire

// acquireFlock takes an exclusive flock on f. Stub: it blocks without end and
// ignores ctx until the bounded wait lands.
func acquireFlock(_ context.Context, f *os.File, label string) (*filelock.Lock, error) {
	return flockAcquire(f, filelock.Options{Mode: filelock.Exclusive, Label: label})
}
