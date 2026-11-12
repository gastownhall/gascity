package main

import (
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

// acquireManagedDoltLifecycleLockWithin takes the lifecycle lock on f,
// waiting at most timeout for a concurrent holder to release it. The lock is
// released when f is closed.
func acquireManagedDoltLifecycleLockWithin(f *os.File, timeout time.Duration) error {
	if f == nil {
		return fmt.Errorf("nil managed dolt lock file")
	}
	if _, err := filelock.Acquire(f, filelock.Options{
		Mode:    filelock.Exclusive,
		Timeout: timeout,
		Label:   f.Name(),
	}); err != nil {
		return fmt.Errorf("lock managed dolt lifecycle: %w", err)
	}
	return nil
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
