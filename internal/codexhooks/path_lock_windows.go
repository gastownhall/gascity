//go:build windows

package codexhooks

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
	"golang.org/x/sys/windows"
)

func withExclusiveOSPathLock(parent, lockName string, fn func() error) error {
	lockPath := filepath.Join(parent, lockName)
	if info, err := os.Lstat(lockPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked Codex hooks lock %s", lockPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspecting Codex hooks lock %s: %w", lockPath, err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening Codex hooks lock %s: %w", lockPath, err)
	}
	defer lockFile.Close() //nolint:errcheck
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(lockFile.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("locking Codex hooks path %s: %w", lockPath, err)
	}
	defer windows.UnlockFileEx(windows.Handle(lockFile.Fd()), 0, 1, 0, &overlapped) //nolint:errcheck
	return fn()
}

func writeFileAtomicNoFollowOS(target string, data []byte, perm os.FileMode) error {
	parent := filepath.Dir(target)
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = os.ErrInvalid
		}
		return fmt.Errorf("refusing non-directory or symlinked Codex hooks parent %s: %w", parent, err)
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked Codex hooks path %s", target)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("inspecting Codex hooks path %s: %w", target, err)
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, target, data, perm)
}
