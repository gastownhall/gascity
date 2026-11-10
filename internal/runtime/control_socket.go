package runtime

import (
	"errors"
	"os"
	"syscall"
)

// ControlSocketOutcome is what one dial of a session's unix control socket
// says about the session behind it.
type ControlSocketOutcome int

const (
	// ControlSocketAbsent means the socket is missing or refused the connection.
	ControlSocketAbsent ControlSocketOutcome = iota
	// ControlSocketPresent means the socket accepted the connection.
	ControlSocketPresent
	// ControlSocketUnknown means the dial failed in a way that cannot tell, such as
	// a timeout, EACCES, or EAGAIN (on Linux, a live owner whose accept
	// backlog is full).
	ControlSocketUnknown
)

// ClassifyControlSocketDial classifies one control-socket dial result for the
// providers (subprocess, acp) whose owner holds the listener only while its
// agent runs. An accepted connection is present. A missing socket is absent,
// and so is a refused one: on Linux a unix socket refuses only when no
// listener is bound, as after its owner died. On macOS and the BSDs a full
// accept backlog also refuses, so there a refused dial can hide a busy owner.
func ClassifyControlSocketDial(err error) ControlSocketOutcome {
	switch {
	case err == nil:
		return ControlSocketPresent
	case errors.Is(err, os.ErrNotExist),
		errors.Is(err, syscall.ENOENT),
		errors.Is(err, syscall.ECONNREFUSED):
		return ControlSocketAbsent
	default:
		return ControlSocketUnknown
	}
}

// UnixSocketPathTooLong reports whether path exceeds the sockaddr_un limit.
// Nothing can be bound at such a path, and dialing it fails with EINVAL, so
// callers skip it rather than read that failure as an outage.
func UnixSocketPathTooLong(path string) bool {
	return len(path) > len(syscall.RawSockaddrUnix{}.Path)-1
}
