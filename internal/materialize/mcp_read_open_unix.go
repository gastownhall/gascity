//go:build !windows

package materialize

import (
	"errors"
	"os"
	"syscall"
)

// openRegularNoFollow opens path for reading without following a symbolic link
// at it and without blocking on a FIFO or device there.
func openRegularNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil && errors.Is(err, syscall.ELOOP) {
		return nil, &NotRegularFileError{Path: path, Mode: os.ModeSymlink}
	}
	return f, err
}
