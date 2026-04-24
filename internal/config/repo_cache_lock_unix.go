//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

const (
	repoCacheLockShared    = syscall.LOCK_SH
	repoCacheLockExclusive = syscall.LOCK_EX
)

func withRepoCacheLock(root string, mode int, createRoot bool, fn func() error) error {
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
	if err := syscall.Flock(int(lockFile.Fd()), mode); err != nil {
		return fmt.Errorf("locking repo cache: %w", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}
