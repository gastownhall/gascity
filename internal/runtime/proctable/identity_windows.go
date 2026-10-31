//go:build windows

package proctable

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/windows"
)

// ProcessIdentity returns the process creation time used by SnapshotProcesses
// as pid's start identity.
//
// Windows reuses PIDs quickly and offers no other stable per-process token, so
// creation time is what separates "the process I was looking at" from "whatever
// now holds that number". The encoding is the decimal Unix-nanosecond form
// pidutil.StartTime also produces on Windows, so the two are interchangeable.
//
// An identity is not a liveness answer. A process that has exited keeps
// answering here for as long as any handle holds its kernel object open, which
// is correct — the PID is reserved for that whole time and cannot name anything
// else — but a caller asking whether the process is still RUNNING must wait on
// a handle or ask pidutil.Alive instead.
func ProcessIdentity(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid PID %d", pid)
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if openErrorMeansProcessGone(err) {
			return "", fmt.Errorf("%w: PID %d", ErrProcessGone, pid)
		}
		return "", fmt.Errorf("reading process identity for PID %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	creation, err := handleCreationTime(handle)
	if err != nil {
		return "", fmt.Errorf("reading process identity for PID %d: %w", pid, err)
	}
	return strconv.FormatInt(creation, 10), nil
}

// openErrorMeansProcessGone reports whether an OpenProcess failure proves the
// process no longer exists.
//
// Only the errors the kernel returns for an unknown PID qualify. Access denied
// must never land here: a protected or other-user process is very much alive,
// and callers read ErrProcessGone as permission to stop worrying about a target
// — including permission to start a replacement that would then race it.
//
// A process that has exited but whose object is still referenced by an open
// handle keeps answering OpenProcess, and its creation time stays readable.
// That is the Windows analogue of a zombie and is deliberately NOT reported as
// gone: the identity it still yields is exactly what fences a later kill.
func openErrorMeansProcessGone(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_NOT_FOUND)
}
