//go:build windows

package runtime

import (
	"errors"
	"syscall"
)

// Winsock codes for the two ways an AF_UNIX connect reports that no listener
// is bound at a pathname. Go's syscall.ECONNREFUSED on Windows is a synthetic
// value no Winsock call returns, so ClassifyControlSocketDial's portable checks
// never match there. Dialing a pathname with no listener reports
// WSAECONNREFUSED, and dialing through a directory that does not exist reports
// WSAENETDOWN; AF_UNIX has no network, so both mean the endpoint is not there.
const (
	wsaeNetDown     = syscall.Errno(10050)
	wsaeConnRefused = syscall.Errno(10061)
)

// platformControlSocketAbsent reports the Windows dial errors that mean no
// listener is bound at the control socket's pathname.
func platformControlSocketAbsent(err error) bool {
	return errors.Is(err, wsaeConnRefused) || errors.Is(err, wsaeNetDown)
}
