//go:build linux

package relay

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Termios ioctls.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// OpenPTY opens a pseudo-terminal pair: the master, which the relay reads
// the program's output from and writes its input to, and the slave, which
// becomes the program's terminal. Neither becomes the caller's controlling
// terminal.
func OpenPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening /dev/ptmx: %w", err)
	}
	var n uint32
	err = control(master, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return fmt.Errorf("unlocking pty: %w", err)
		}
		var err error
		if n, err = unix.IoctlGetUint32(fd, unix.TIOCGPTN); err != nil {
			return fmt.Errorf("naming pty: %w", err)
		}
		return nil
	})
	if err != nil {
		_ = master.Close() // the open pair is abandoned; its error adds nothing
		return nil, nil, err
	}
	name := "/dev/pts/" + strconv.FormatUint(uint64(n), 10)
	slave, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close() // the open pair is abandoned; its error adds nothing
		return nil, nil, fmt.Errorf("opening %s: %w", name, err)
	}
	return master, slave, nil
}
