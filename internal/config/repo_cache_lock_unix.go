//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	repoCacheLockShared    = syscall.LOCK_SH
	repoCacheLockExclusive = syscall.LOCK_EX
)

func withRepoCacheLock(root string, opts repoCacheLockOptions, fn func() error) error {
	createRoot := opts.createRoot
	if createRoot {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("creating repo cache root: %w", err)
		}
	} else if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return fn()
		}
		return fmt.Errorf("checking repo cache root: %w", err)
	}
	lockPath := root + string(os.PathSeparator) + repoCacheLockName
	flags := os.O_RDWR | os.O_CREATE
	lockFile, err := os.OpenFile(lockPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("opening repo cache lock file: %w", err)
	}
	defer lockFile.Close() //nolint:errcheck
	mode := opts.mode
	if opts.nonBlocking {
		mode |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(lockFile.Fd()), mode); err != nil {
		// LOCK_NB reports contention as EWOULDBLOCK. Everything else is a
		// real failure and must not be mistaken for a busy cache.
		if opts.nonBlocking && errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrRepoCacheBusy
		}
		return fmt.Errorf("locking repo cache: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}
