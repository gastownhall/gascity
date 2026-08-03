//go:build productmetrics_testhook && darwin && !ios

package main

import (
	"bytes"
	"fmt"
	"os"
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

	const ioctlParameterMask = 0x1fff
	const terminalNameLength = (syscall.TIOCPTYGNAME >> 16) & ioctlParameterMask
	terminalName := make([]byte, terminalNameLength)
	if err := productMetricsControlPTYIoctl(output, "TIOCPTYGNAME", syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&terminalName[0]))); err != nil {
		return nil, nil, err
	}
	if err := productMetricsControlPTYIoctl(output, "TIOCPTYGRANT", syscall.TIOCPTYGRANT, 0); err != nil {
		return nil, nil, err
	}
	if err := productMetricsControlPTYIoctl(output, "TIOCPTYUNLK", syscall.TIOCPTYUNLK, 0); err != nil {
		return nil, nil, err
	}
	nullIndex := bytes.IndexByte(terminalName, 0)
	if nullIndex <= 0 {
		return nil, nil, fmt.Errorf("PTY terminal name is not NUL-terminated")
	}
	terminal, err = os.OpenFile(string(terminalName[:nullIndex]), os.O_RDWR|syscall.O_NOCTTY, 0)
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
