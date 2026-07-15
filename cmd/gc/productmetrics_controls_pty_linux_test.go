//go:build linux && !android

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

func openProductMetricsControlPTY() (output, terminal *os.File, returnErr error) {
	output, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open PTY multiplexer: %w", err)
	}
	defer func() {
		if returnErr != nil {
			_ = output.Close()
		}
	}()

	var number uint32
	if err := productMetricsControlPTYIoctl(output, "TIOCGPTN", syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); err != nil {
		return nil, nil, err
	}
	var locked int
	if err := productMetricsControlPTYIoctl(output, "TIOCSPTLCK", syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&locked))); err != nil {
		return nil, nil, err
	}
	terminal, err = os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(number), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open PTY terminal: %w", err)
	}
	return output, terminal, nil
}

func productMetricsControlPTYIoctl(file *os.File, name string, command, pointer uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), command, pointer)
	if errno != 0 {
		return fmt.Errorf("%s PTY ioctl: %w", name, errno)
	}
	return nil
}
