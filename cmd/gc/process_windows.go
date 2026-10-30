//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func platformGetpgid(pid int) (int, error) {
	return pid, nil
}

func platformGetpgrp() int {
	return os.Getpid()
}

func platformKill(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to terminate unsafe Windows PID %d", pid)
	}
	if sig != syscall.SIGKILL {
		return fmt.Errorf("signal %v for PID %d: %w", sig, pid, errSignalUnsupported)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
