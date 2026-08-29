//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const (
	repoCacheLockShared    = 0
	repoCacheLockExclusive = 1
)

func withRepoCacheLock(root string, opts repoCacheLockOptions, fn func() error) error {
	createRoot := opts.createRoot
	if createRoot {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("creating repo cache root: %w", err)
		}
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) && !createRoot {
			return fn()
		}
		return fmt.Errorf("opening repo cache lock root: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(root, repoCacheLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening repo cache lock: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck

	var flags uint32
	if opts.mode == repoCacheLockExclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if opts.nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(lockFile.Fd()), flags, 0, 1, 0, &overlapped); err != nil {
		// LOCKFILE_FAIL_IMMEDIATELY reports contention as ERROR_LOCK_VIOLATION.
		// Everything else is a real failure and must not be mistaken for a
		// busy cache.
		if opts.nonBlocking && errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return ErrRepoCacheBusy
		}
		return fmt.Errorf("locking repo cache: %w", err)
	}
	defer windows.UnlockFileEx(windows.Handle(lockFile.Fd()), 0, 1, 0, &overlapped) //nolint:errcheck
	return fn()
}
