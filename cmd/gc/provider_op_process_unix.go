//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareProviderOpCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd == nil || cmd.Process == nil {
			return nil
		}
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err == nil {
			if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil &&
				!errors.Is(killErr, os.ErrProcessDone) &&
				!errors.Is(killErr, syscall.ESRCH) {
				return killErr
			}
			return nil
		}
		if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return killErr
		}
		return nil
	}
}
