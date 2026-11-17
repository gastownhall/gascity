//go:build windows

package session

import (
	"errors"
	"os"

	"github.com/gastownhall/gascity/internal/platform/filelock"
	"golang.org/x/sys/windows"
)

// runtimeLeaseFileID returns the volume serial number and file index of the
// open lock file f, Windows' counterpart of a device and inode, and whether
// they could be read.
func runtimeLeaseFileID(f *os.File, _ os.FileInfo) (dev, ino uint64, ok bool) {
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &d); err != nil {
		return 0, 0, false
	}
	return uint64(d.VolumeSerialNumber), uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow), true
}

// tryLockRuntimeNameFile takes f's exclusive lock without blocking. busy
// reports that another holder has it. The lock lives as long as f's handle.
func tryLockRuntimeNameFile(f *os.File) (busy bool, err error) {
	if _, err := filelock.Acquire(f, filelock.Options{Mode: filelock.Exclusive, NonBlocking: true}); err != nil {
		return errors.Is(err, filelock.ErrBusy), err
	}
	return false, nil
}

// releaseRuntimeNameFileLock is a no-op: closing f releases its lock.
func releaseRuntimeNameFileLock(*os.File) {}
