//go:build windows

package packregistry

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func withExclusiveFileLock(lockFile *os.File, label string, fn func() error) error {
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(lockFile.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("acquiring %s: %w", label, err)
	}
	defer windows.UnlockFileEx(windows.Handle(lockFile.Fd()), 0, 1, 0, &overlapped) //nolint:errcheck
	return fn()
}
