package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
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
// long as ctx allows. It tries first without blocking, so a free lock acquires
// even when ctx is already done: ctx bounds the wait, not the attempt. Only
// contention is waited out, one flockPollInterval at a time; EINTR retries at
// once and any other error returns at once. When ctx ends first it returns
// ctx.Err(); callers that own the wait add ErrStoreTimeout (mapStoreTimeout).
// A context that can never end keeps the plain blocking acquire. A context
// already done makes this exactly one attempt.
func flockExclusive(ctx context.Context, f *os.File, label string) (*filelock.Lock, error) {
	if ctx.Done() == nil {
		for {
			lock, err := flockAcquire(f, filelock.Options{Mode: filelock.Exclusive, Label: label})
			if !errors.Is(err, syscall.EINTR) {
				return lock, err
			}
		}
	}
	for {
		lock, err := tryFlockExclusive(f, label)
		switch {
		case err == nil:
			return lock, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, filelock.ErrBusy):
			return nil, err
		}
		if err := waitFlockPoll(ctx); err != nil {
			return nil, err
		}
	}
}

// waitFlockPoll sleeps one flockPollInterval and reports ctx.Err() when ctx
// ends first or is already done.
func waitFlockPoll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(flockPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// tryFlockExclusive makes exactly one non-blocking attempt at an exclusive lock
// on f and reports contention as filelock.ErrBusy.
func tryFlockExclusive(f *os.File, label string) (*filelock.Lock, error) {
	return flockAcquire(f, filelock.Options{Mode: filelock.Exclusive, NonBlocking: true, Label: label})
}

// mapStoreTimeout turns the end of a store-owned wait into ErrStoreTimeout.
// waitCtx is the context that bounded the wait and parent the one it derives
// from. err comes back unchanged when it is nil, when the wait is still live,
// when the parent is done (a caller's own cancel is never the store's timeout)
// or when err already carries ErrStoreTimeout. Otherwise the result matches
// ErrStoreTimeout, the wait context's error and err. waitCtx is read before
// parent: a done parent always ends its child first, so this order cannot take
// a caller's cancel for a timeout.
func mapStoreTimeout(waitCtx, parent context.Context, err error) error {
	if err == nil || waitCtx.Err() == nil || parent.Err() != nil || errors.Is(err, ErrStoreTimeout) {
		return err
	}
	return fmt.Errorf("%w: %w: %w", ErrStoreTimeout, waitCtx.Err(), err)
}
