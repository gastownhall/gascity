//go:build !windows

package packregistry

import (
	"fmt"
	"os"
	"syscall"
)

func withExclusiveFileLock(lockFile *os.File, label string, fn func() error) error {
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquiring %s: %w", label, err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}
