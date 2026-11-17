//go:build windows

package subprocess

import (
	"errors"
	"os"
	"syscall"
)

// Winsock codes for the two ways an AF_UNIX connect reports that no session is
// listening at a pathname. Go's syscall package names neither, and its
// syscall.ECONNREFUSED on Windows is a synthetic APPLICATION_ERROR value that
// no Winsock call ever returns, so matching against it can only ever be false.
//
// Both values were measured on this platform rather than assumed, by
// TestUnavailableSocketErrorsCoverWindowsDialFailures: dialling a pathname
// with no listener — whether the socket file is absent or the name belongs to
// an ordinary file — reports WSAECONNREFUSED, and dialling through a directory
// that does not exist reports WSAENETDOWN. Neither is a network fault here;
// AF_UNIX has no network. Both mean the endpoint is not there.
const (
	wsaeNetDown     = syscall.Errno(10050)
	wsaeConnRefused = syscall.Errno(10061)
)

// isUnavailableSocketError reports whether err means no session is listening
// at the pathname, rather than a fault the caller has to surface.
//
// Stop and Interrupt are idempotent, and that rests entirely on this
// distinction: a dial that proves the session is already gone must read as
// success, while anything else is a failure the caller must see.
func isUnavailableSocketError(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, wsaeConnRefused) ||
		errors.Is(err, wsaeNetDown)
}
