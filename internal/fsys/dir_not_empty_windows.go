//go:build windows

package fsys

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// IsDirNotEmpty reports whether err says a directory could not be removed
// because it still has entries.
//
// Windows reports that as ERROR_DIR_NOT_EMPTY. syscall.ENOTEMPTY is an errno Go
// invents on Windows, so the kernel never returns it; it is still accepted so
// an error constructed from it classifies the same on every platform.
func IsDirNotEmpty(err error) bool {
	return errors.Is(err, windows.ERROR_DIR_NOT_EMPTY) || errors.Is(err, syscall.ENOTEMPTY)
}
