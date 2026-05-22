//go:build !windows

// Package processgroup provides Unix process-group cleanup helpers.
package processgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const defaultPollPeriod = 25 * time.Millisecond

// Options configures process-group cleanup.
type Options struct {
	Kill           func(pid int, sig syscall.Signal) error
	CurrentGroupID func() int
	PollPeriod     time.Duration
}

func (o Options) kill(pid int, sig syscall.Signal) error {
	if o.Kill != nil {
		return o.Kill(pid, sig)
	}
	return syscall.Kill(pid, sig)
}

func (o Options) currentGroupID() int {
	if o.CurrentGroupID != nil {
		return o.CurrentGroupID()
	}
	return syscall.Getpgrp()
}

func (o Options) pollPeriod() time.Duration {
	if o.PollPeriod > 0 {
		return o.PollPeriod
	}
	return defaultPollPeriod
}

// StartCommandInNewGroup configures cmd to start as a new process-group leader.
func StartCommandInNewGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Terminate sends SIGTERM to pgid, waits for exit, then escalates to SIGKILL.
func Terminate(pgid int, timeout time.Duration, opts Options) error {
	if pgid <= 1 || pgid == opts.currentGroupID() {
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := opts.kill(-pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if err := waitForExit(pgid, timeout, opts); err == nil {
		return nil
	}
	if err := opts.kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return waitForExit(pgid, timeout, opts)
}

// TerminateCommand terminates cmd's process group and falls back to direct kill.
func TerminateCommand(cmd *exec.Cmd, knownPGID int, timeout time.Duration, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := knownPGID
	if pgid <= 0 {
		resolved, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return nil
			}
			return killDirect(cmd.Process, fmt.Errorf("resolve process group for pid %d: %w", cmd.Process.Pid, err))
		}
		pgid = resolved
	}
	if err := Terminate(pgid, timeout, opts); err != nil {
		return killDirect(cmd.Process, fmt.Errorf("terminate process group %d: %w", pgid, err))
	}
	return nil
}

func waitForExit(pgid int, timeout time.Duration, opts Options) error {
	deadline := time.Now().Add(timeout)
	for {
		if !alive(pgid, opts) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process group %d did not exit within %s", pgid, timeout)
		}
		time.Sleep(opts.pollPeriod())
	}
}

func alive(pgid int, opts Options) bool {
	if pgid <= 0 {
		return false
	}
	err := opts.kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killDirect(process *os.Process, cause error) error {
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if cause != nil {
			return fmt.Errorf("%w; direct kill failed: %w", cause, err)
		}
		return err
	}
	return cause
}
