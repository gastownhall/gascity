//go:build !windows

package session

import (
	"errors"
	"os"
	"syscall"
)

// runtimeLeaseFileID returns the device and inode of the open lock file whose
// stat is fi, and whether they could be read.
func runtimeLeaseFileID(_ *os.File, fi os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), st.Ino, true //nolint:unconvert // Dev is int32 on darwin
}

// tryLockRuntimeNameFile takes f's exclusive lock without blocking. busy
// reports that another holder has it.
func tryLockRuntimeNameFile(f *os.File) (busy bool, err error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK), err
	}
	return false, nil
}

// releaseRuntimeNameFileLock drops f's lock; closing f releases it too.
func releaseRuntimeNameFileLock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // close releases it too
}
