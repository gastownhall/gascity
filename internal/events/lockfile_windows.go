//go:build windows

package events

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile acquires an exclusive file lock on f via LockFileEx. This is
// the Windows equivalent of flock(LOCK_EX) and coordinates across
// processes on the same file.
func lockFile(f *os.File) error {
	handle := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	// Lock the maximum possible range so the entire file is covered.
	return windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		^uint32(0),
		^uint32(0),
		ol,
	)
}

// unlockFile releases a lock previously acquired with lockFile.
func unlockFile(f *os.File) error {
	handle := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(
		handle,
		0,
		^uint32(0),
		^uint32(0),
		ol,
	)
}
