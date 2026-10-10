//go:build linux

package events

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames oldpath to newpath atomically, failing with an
// error that matches fs.ErrExist instead of replacing an existing newpath. A
// kernel or filesystem without RENAME_NOREPLACE gets renameIfAbsent.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return renameIfAbsent(oldpath, newpath)
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
