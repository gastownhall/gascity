package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/platform/filelock"
)

func openManagedDoltLifecycleLock(cityPath string) (*os.File, managedDoltRuntimeLayout, error) {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, err
	}
	f, err := openManagedDoltLifecycleLockAt(layout)
	if err != nil {
		return nil, managedDoltRuntimeLayout{}, err
	}
	return f, layout, nil
}

// openManagedDoltLifecycleLockAt opens (creating if needed) the lifecycle
// lock file of an already-resolved layout without locking it.
func openManagedDoltLifecycleLockAt(layout managedDoltRuntimeLayout) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(layout.LockFile), 0o755); err != nil {
		return nil, fmt.Errorf("create managed dolt lock dir: %w", err)
	}
	f, err := os.OpenFile(layout.LockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open managed dolt lock: %w", err)
	}
	return f, nil
}

// managedDoltLifecycleLockPoll is how often a bounded wait retries a held
// lifecycle lock.
const managedDoltLifecycleLockPoll = 10 * time.Millisecond

// errManagedDoltLifecycleBusy reports that another managed dolt lifecycle
// operation (a recovery, start or stop) still held the lifecycle lock when a
// bounded wait for it ended.
var errManagedDoltLifecycleBusy = errors.New("another managed dolt lifecycle operation is in flight")

// acquireManagedDoltLifecycleLockContext takes the lifecycle lock on f,
// waiting at most timeout and never past ctx. A holder that keeps the lock
// for the whole wait yields an error wrapping errManagedDoltLifecycleBusy,
// and an ended ctx an error wrapping ctx's error. The lock is released when
// f is closed.
func acquireManagedDoltLifecycleLockContext(ctx context.Context, f *os.File, timeout time.Duration) error {
	if f == nil {
		return fmt.Errorf("nil managed dolt lock file")
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for managed dolt lifecycle lock %s: %w", f.Name(), err)
		}
		if locked, err := tryManagedDoltLifecycleLock(f); err != nil || locked {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("%w: %s is still held after %s", errManagedDoltLifecycleBusy, f.Name(), timeout)
		}
		retry := time.NewTimer(min(managedDoltLifecycleLockPoll, remaining))
		select {
		case <-ctx.Done():
			retry.Stop()
		case <-retry.C:
		}
	}
}

func tryManagedDoltLifecycleLock(f *os.File) (bool, error) {
	if f == nil {
		return false, fmt.Errorf("nil managed dolt lock file")
	}
	_, err := filelock.Acquire(f, filelock.Options{
		Mode:        filelock.Exclusive,
		NonBlocking: true,
		Label:       f.Name(),
	})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, filelock.ErrBusy) {
		return false, nil
	}
	return false, fmt.Errorf("lock managed dolt lifecycle: %w", err)
}

func releaseManagedDoltLifecycleLock(f *os.File) {
	if f == nil {
		return
	}
	_ = f.Close()
}
