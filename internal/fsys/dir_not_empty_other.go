//go:build !windows

package fsys

import (
	"errors"
	"syscall"
)

// IsDirNotEmpty reports whether err says a directory could not be removed
// because it still has entries.
func IsDirNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}
