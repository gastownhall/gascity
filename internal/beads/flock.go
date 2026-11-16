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

// flockAcquire is the kernel lock call, a seam for tests: it lets a unit test
// script contention and other errors without a second process.
var flockAcquire = filelock.Acquire

// acquireFlock takes an exclusive flock on f. It tries without blocking first,
// so a free lock is acquired even when ctx is already done: ctx bounds only the
// wait. Contention (filelock.ErrBusy) waits one flockPollInterval or until ctx
// ends and tries again; any other error returns at once. A ctx that can never
// end keeps the plain blocking acquisition. When ctx ends the wait returns
// ctx.Err(); the caller adds ErrStoreTimeout.
func acquireFlock(ctx context.Context, f *os.File, label string) (*filelock.Lock, error) {
	opts := filelock.Options{Mode: filelock.Exclusive, Label: label}
	if ctx.Done() == nil {
		return flockAcquire(f, opts)
	}
	opts.NonBlocking = true
	for {
		lock, err := flockAcquire(f, opts)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, filelock.ErrBusy) {
			return nil, err
		}
		if err := waitFlockPoll(ctx); err != nil {
			return nil, err
		}
	}
}

// waitFlockPoll waits one flockPollInterval, or returns ctx.Err() as soon as
// ctx ends.
func waitFlockPoll(ctx context.Context) error {
	timer := time.NewTimer(flockPollInterval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
