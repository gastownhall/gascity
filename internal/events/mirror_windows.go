//go:build windows

package events

import (
	"errors"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/platform/filelock"
	"github.com/gastownhall/gascity/internal/platform/livefile"
	"golang.org/x/sys/windows"
)

// errMirrorSymlink is openMirrorPath refusing a reparse point at the path.
var errMirrorSymlink = errors.New("the path is a symbolic link or other reparse point")

// openMirrorPath opens path, refusing a symbolic link or other reparse point
// at it. Windows has no O_NOFOLLOW, so the path is checked just before the
// open; checkOpened then refuses anything but a regular file.
func openMirrorPath(path string, flag int, perm os.FileMode) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: errMirrorSymlink}
	}
	return livefile.OpenFile(path, flag, perm)
}

// isMirrorSymlinkRefusal reports whether err is openMirrorPath refusing a
// reparse point.
func isMirrorSymlinkRefusal(err error) bool {
	return errors.Is(err, errMirrorSymlink)
}

// mirrorLinkCount returns the hard-link count of the opened file f and
// whether it could be read.
func mirrorLinkCount(f *os.File, _ os.FileInfo) (uint64, bool) {
	var d windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &d); err != nil {
		return 0, false
	}
	return uint64(d.NumberOfLinks), true
}

// tryLockMirror takes the exclusive, nonblocking mirror lock at path and
// returns the function that releases it.
func tryLockMirror(path string) (func(), error) {
	lock, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	held, err := filelock.Acquire(lock, filelock.Options{Mode: filelock.Exclusive, NonBlocking: true})
	if err != nil {
		_ = lock.Close()
		if errors.Is(err, filelock.ErrBusy) {
			return nil, errMirrorLockHeld
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() {
		_ = held.Release()
		_ = lock.Close()
	}, nil
}
