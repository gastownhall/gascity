//go:build unix

package tailfile

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// pollRDBand is POLLRDBAND, 0x80 on Linux, macOS and the BSDs; x/sys/unix
// does not define it for every one of them.
const pollRDBand = 0x80

// outputGone returns an error wrapping syscall.EPIPE when out is a pipe whose
// reader has closed, as tail -f checks its standard output. A follow of a
// quiet file writes nothing, so it would never learn this from a write and
// would run until killed, holding the pipeline open behind it. Any other
// writer, or one that cannot be checked, is taken as live.
func outputGone(out io.Writer) error {
	f, ok := out.(*os.File)
	if !ok {
		return nil
	}
	if info, err := f.Stat(); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		return nil
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return nil
	}
	var revents int16
	_ = raw.Control(func(fd uintptr) {
		// POLLRDBAND is requested, as tail does, because some systems report
		// a pipe's error only for a descriptor polled for something.
		fds := []unix.PollFd{{Fd: int32(fd), Events: pollRDBand}}
		if n, err := unix.Poll(fds, 0); err == nil && n > 0 {
			revents = fds[0].Revents
		}
	})
	if revents&(unix.POLLERR|unix.POLLHUP) != 0 {
		return fmt.Errorf("writing output: %w", syscall.EPIPE)
	}
	return nil
}
