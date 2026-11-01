//go:build linux

package dolttest

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// waitProcessExit blocks until the process identified by pid and startTime
// exits, or timeout passes, and reports whether it exited. It waits on a
// pidfd, which becomes readable when the process terminates whether or not
// it is a child of this one, so no polling interval sits between the exit
// and the return.
func waitProcessExit(pid int, startTime string, timeout time.Duration) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		// ESRCH: the pid is already gone. Any other failure leaves only the
		// identity probe to answer.
		return errors.Is(err, unix.ESRCH) || !pidutil.AliveWithStartTime(pid, startTime)
	}
	defer unix.Close(fd) //nolint:errcheck
	// The pid may have been recycled before the pidfd pinned it; a different
	// process under the same number means the target already exited.
	if !pidutil.AliveWithStartTime(pid, startTime) {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(remaining.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return !pidutil.AliveWithStartTime(pid, startTime)
		}
		if n > 0 {
			return true
		}
	}
}
