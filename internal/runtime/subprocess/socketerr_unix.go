//go:build !windows

package subprocess

import (
	"errors"
	"os"
	"syscall"
)

// isUnavailableSocketError reports whether err means no session is listening
// at the pathname, rather than a fault the caller has to surface.
//
// Stop and Interrupt are idempotent, and that rests entirely on this
// distinction: a missing socket file and a refused connection both prove the
// session is already gone, while anything else is a failure the caller must
// see instead of reading as success.
func isUnavailableSocketError(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ECONNREFUSED)
}
