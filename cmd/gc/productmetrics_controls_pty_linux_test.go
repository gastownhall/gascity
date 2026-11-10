//go:build productmetrics_testhook && linux && !android

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
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

// productMetricsControlPTYPeerUnsupported reports whether a TIOCGPTPEER errno
// means the kernel lacks the ioctl, so the path-based open is the fallback.
func productMetricsControlPTYPeerUnsupported(_ syscall.Errno) bool {
	return false
}

// openProductMetricsControlPTYPeer returns the slave of master's pty.
func openProductMetricsControlPTYPeer(_ *os.File, _ uint32) (*os.File, error) {
	return nil, errors.New("not implemented")
}

func TestProductMetricsControlPTYPeer(t *testing.T) {
	t.Run("falls back to the path only on kernels without TIOCGPTPEER", func(t *testing.T) {
		for errno, want := range map[syscall.Errno]bool{
			syscall.ENOTTY: true,
			syscall.EINVAL: true,
			syscall.EACCES: false,
			syscall.EPERM:  false,
			syscall.EBADF:  false,
			syscall.EIO:    false,
		} {
			if got := productMetricsControlPTYPeerUnsupported(errno); got != want {
				t.Errorf("productMetricsControlPTYPeerUnsupported(%v) = %t, want %t", errno, got, want)
			}
		}
	})

	t.Run("peer ioctl returns the master's slave", func(t *testing.T) {
		master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open PTY multiplexer: %v", err)
		}
		defer func() { _ = master.Close() }()
		var number uint32
		if err := productMetricsControlPTYIoctl(master, "TIOCGPTN", syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number))); err != nil {
			t.Fatal(err)
		}
		var locked int
		if err := productMetricsControlPTYIoctl(master, "TIOCSPTLCK", syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&locked))); err != nil {
			t.Fatal(err)
		}
		terminal, err := openProductMetricsControlPTYPeer(master, number)
		if err != nil {
			t.Fatalf("open peer: %v", err)
		}
		defer func() { _ = terminal.Close() }()
		assertProductMetricsControlPTYRoundTrip(t, master, terminal)
	})

	t.Run("openProductMetricsControlPTY pairs output with terminal", func(t *testing.T) {
		output, terminal, err := openProductMetricsControlPTY()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = output.Close() }()
		defer func() { _ = terminal.Close() }()
		assertProductMetricsControlPTYRoundTrip(t, output, terminal)
	})
}

// assertProductMetricsControlPTYRoundTrip writes a line to terminal and reads
// it back from output, proving the two are one pty pair.
func assertProductMetricsControlPTYRoundTrip(t *testing.T, output, terminal *os.File) {
	t.Helper()
	const line = "pty-peer-round-trip"
	if _, err := terminal.WriteString(line + "\n"); err != nil {
		t.Fatalf("write terminal: %v", err)
	}
	got := make(chan []byte, 1)
	go func() {
		var seen []byte
		buf := make([]byte, 256)
		for !bytes.Contains(seen, []byte(line)) {
			n, err := output.Read(buf)
			seen = append(seen, buf[:n]...)
			if err != nil {
				break
			}
		}
		got <- seen
	}()
	select {
	case seen := <-got:
		if !bytes.Contains(seen, []byte(line)) {
			t.Fatalf("output read %q, want it to contain %q", seen, line)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out reading %q back from output", line)
	}
}
