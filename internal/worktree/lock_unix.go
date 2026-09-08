//go:build !windows

package worktree

import (
	"fmt"
	"os"
	"syscall"
)

// pathLock holds an exclusive advisory lock on one workspace path.
type pathLock struct{ f *os.File }

// lockPath acquires an exclusive lock for the given workspace path within the
// given repository, blocking until it is available. The returned lock must be
// released with unlock.
func lockPath(repoDir, path string) (*pathLock, error) {
	f, err := openLockFile(repoDir, path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking worktree %q: %w", path, err)
	}
	return &pathLock{f: f}, nil
}

// unlock releases the lock. It is safe to call on a nil lock so callers can
// defer it unconditionally.
func (l *pathLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
