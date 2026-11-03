//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func prepareProviderOpCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd == nil || cmd.Process == nil {
			return nil
		}
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err == nil {
			if killErr := syscall.Kill(-pgid, syscall.SIGKILL); killErr != nil &&
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

// runProviderOpCommand runs cmd in its own process group, which its context
// ending kills whole (prepareProviderOpCommand).
func runProviderOpCommand(cmd *exec.Cmd) error {
	prepareProviderOpCommand(cmd)
	return cmd.Run()
}
