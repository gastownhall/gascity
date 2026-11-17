//go:build !windows

package events

import (
	"errors"
	"os"
	"syscall"
)

// mirrorNoFollowFlags make an open refuse a symbolic link at the path and not
// block on a FIFO or device there.
const mirrorNoFollowFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// openMirrorPath opens path without following a symbolic link at it.
func openMirrorPath(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|mirrorNoFollowFlags, perm)
}

// isMirrorSymlinkRefusal reports whether err is openMirrorPath refusing a
// symbolic link.
func isMirrorSymlinkRefusal(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

// mirrorLinkCount returns the hard-link count of the opened file f, whose
// stat is info, and whether it could be read.
func mirrorLinkCount(_ *os.File, info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true //nolint:unconvert // Nlink is uint16 on darwin
}

// tryLockMirror takes the exclusive, nonblocking mirror lock at path and
// returns the function that releases it.
func tryLockMirror(path string) (func(), error) {
	lock, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errMirrorLockHeld
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}
