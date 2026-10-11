//go:build windows

package worktree

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// pathLock holds an exclusive lock on one workspace path. The overlapped value
// is retained because Windows requires the same one to release the range it was
// acquired with.
type pathLock struct {
	f  *os.File
	ov windows.Overlapped
}

// lockPath acquires an exclusive lock for the given workspace path within the
// given repository, blocking until it is available. The returned lock must be
// released with unlock.
func lockPath(repoDir, path string) (*pathLock, error) {
	f, err := openLockFile(repoDir, path)
	if err != nil {
		return nil, err
	}
	lock := &pathLock{f: f}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &lock.ov); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking worktree %q: %w", path, err)
	}
	return lock, nil
}

// unlock releases the lock. It is safe to call on a nil lock so callers can
// defer it unconditionally.
func (l *pathLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &l.ov)
	_ = l.f.Close()
	l.f = nil
}
