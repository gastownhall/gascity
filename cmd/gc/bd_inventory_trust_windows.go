//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"

	"github.com/gastownhall/gascity/internal/platform/filelock"
	"github.com/gastownhall/gascity/internal/processgroup"
	"golang.org/x/sys/windows"
)

// statBdBinaryFile identifies the resolved bd binary at abs by its volume
// serial number and file index (Windows' device and inode), size, last write
// time and creation time.
func statBdBinaryFile(abs string) (bdBinaryID, error) {
	f, err := os.Open(abs)
	if err != nil {
		return bdBinaryID{}, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &d); err != nil {
		return bdBinaryID{}, &os.PathError{Op: "stat", Path: abs, Err: err}
	}
	return bdBinaryID{
		Path:  abs,
		Dev:   uint64(d.VolumeSerialNumber),
		Ino:   uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow),
		Size:  int64(d.FileSizeHigh)<<32 | int64(d.FileSizeLow),
		MTime: d.LastWriteTime.Nanoseconds(),
		CTime: d.CreationTime.Nanoseconds(),
	}, nil
}

// tryLockBdTrustCacheFile takes f's exclusive lock without blocking and
// returns the function that releases it and closes f. busy reports that
// another holder has the lock.
func tryLockBdTrustCacheFile(f *os.File) (release func(), busy bool, err error) {
	lock, err := filelock.Acquire(f, filelock.Options{Mode: filelock.Exclusive, NonBlocking: true})
	if err != nil {
		return nil, errors.Is(err, filelock.ErrBusy), err
	}
	return func() {
		_ = lock.Release()
		_ = f.Close()
	}, false, nil
}

// cancelBdCommandAsGroup starts cmd in its own process group; its context
// cancel keeps exec's default, killing the bd process.
func cancelBdCommandAsGroup(cmd *exec.Cmd) {
	processgroup.StartCommandInNewGroup(cmd)
}
