//go:build unix

package bazeltest

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, blocking until acquired.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases the advisory lock taken by lockFile.
func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
