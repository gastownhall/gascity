//go:build unix

package events

import (
	"os"
	"syscall"
)

// lockFile acquires an exclusive advisory lock on f via flock(LOCK_EX).
// The lock is released when unlockFile is called or the file descriptor
// is closed. This is a kernel-level lock and coordinates across
// processes on the same file.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases an advisory lock previously taken with lockFile.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
