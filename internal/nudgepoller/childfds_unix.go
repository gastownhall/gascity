//go:build unix

package nudgepoller

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"golang.org/x/sys/unix"
)

// firstExtraFD is the descriptor number os/exec gives ExtraFiles[0]; 0-2 are
// stdin, stdout and stderr.
const firstExtraFD = 3

// ScrubInheritedFDs makes the child of cmd start without any descriptor above
// stderr that the calling process holds open without close-on-exec.
//
// The poller outlives its spawner by design. os/exec never closes inherited
// descriptors that lack close-on-exec, so one a caller left behind (a shell's
// `exec 9>lock; flock 9`, a parent that opened a file without O_CLOEXEC)
// would ride into the poller, and because a flock belongs to the open file
// description, it would stay held until the poller exits, long after the
// caller released it. Descriptors Go itself opens are close-on-exec and need
// no help; this covers the ones opened outside Go.
//
// It lists the process's open descriptors and passes a nil ExtraFiles entry
// for each slot up to the highest one lacking close-on-exec; os/exec closes a
// nil entry's descriptor in the child before exec. ScrubInheritedFDs owns
// cmd.ExtraFiles, so it returns an error when the caller already set it.
func ScrubInheritedFDs(cmd *exec.Cmd) error {
	if len(cmd.ExtraFiles) != 0 {
		return fmt.Errorf("scrubbing inherited fds: command already passes %d extra files", len(cmd.ExtraFiles))
	}
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return fmt.Errorf("scrubbing inherited fds: listing open descriptors: %w", err)
	}
	highest := firstExtraFD - 1
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < firstExtraFD {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if errors.Is(err, unix.EBADF) {
			continue // the directory handle ReadDir itself held, already closed
		}
		if err != nil {
			return fmt.Errorf("scrubbing inherited fds: reading flags of fd %d: %w", fd, err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			highest = max(highest, fd)
		}
	}
	if highest >= firstExtraFD {
		cmd.ExtraFiles = make([]*os.File, highest-firstExtraFD+1)
	}
	return nil
}
