//go:build darwin

package relay

import (
	"bytes"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Termios ioctls.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// ptyNameLen is the buffer TIOCPTYGNAME fills: the ioctl's parameter size.
const ptyNameLen = (unix.TIOCPTYGNAME >> 16) & 0x1fff

// OpenPTY opens a pseudo-terminal pair: the master, which the relay reads
// the program's output from and writes its input to, and the slave, which
// becomes the program's terminal. Neither becomes the caller's controlling
// terminal.
func OpenPTY() (master, slave *os.File, err error) {
	// The master stays in the runtime poller (os.OpenFile, not a blocking
	// descriptor wrapped by os.NewFile as golang/go#22099 once suggested):
	// hanging the program up and the pane going both rely on Close
	// interrupting a read in flight, which a blocking descriptor's read
	// does not allow.
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening /dev/ptmx: %w", err)
	}
	var name string
	err = control(master, func(fd int) error {
		if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
			return fmt.Errorf("granting pty: %w", err)
		}
		if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
			return fmt.Errorf("unlocking pty: %w", err)
		}
		var err error
		name, err = ptyName(fd)
		return err
	})
	if err != nil {
		_ = master.Close() // the open pair is abandoned; its error adds nothing
		return nil, nil, err
	}
	slave, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close() // the open pair is abandoned; its error adds nothing
		return nil, nil, fmt.Errorf("opening %s: %w", name, err)
	}
	return master, slave, nil
}

// ptyName returns the name of the slave of the pty master fd: what
// ptsname(3) returns, through the TIOCPTYGNAME ioctl it is built on.
func ptyName(fd int) (string, error) {
	buf := make([]byte, ptyNameLen)
	// x/sys/unix wraps no ioctl that fills a caller's buffer of this size,
	// so this is the raw ioctl(2) trap, as creack/pty issues it.
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&buf[0]))) //nolint:staticcheck // SA1019: x/sys/unix has no libSystem wrapper for TIOCPTYGNAME
	if errno != 0 {
		return "", fmt.Errorf("naming pty: %w", errno)
	}
	end := bytes.IndexByte(buf, 0)
	if end <= 0 {
		return "", fmt.Errorf("naming pty: name is not NUL-terminated")
	}
	return string(buf[:end]), nil
}
