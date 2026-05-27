//go:build !windows

package beads

import (
	"os"
	"syscall"
)

func tryBdListReadFileLock(lockFile *os.File) (acquired, retryable bool, err error) {
	flockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if flockErr == nil {
		return true, false, nil
	}
	if flockErr == syscall.EWOULDBLOCK || flockErr == syscall.EAGAIN {
		return false, true, flockErr
	}
	return false, false, flockErr
}

func releaseBdListReadFileLock(lockFile *os.File) {
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
}
