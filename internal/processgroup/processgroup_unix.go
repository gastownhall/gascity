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

// StartCommand starts cmd as the leader of its own process group and returns
// the group's identifier.
//
// Persistence is accepted for a platform-neutral call shape and has no effect
// here: a Unix process group already behaves both ways by construction. The
// group outlives the parent, and cancellation reaches the whole tree by
// signaling -pgid, so there is no handle whose closure could imply a kill.
func StartCommand(cmd *exec.Cmd, _ Persistence) (int, error) {
	if cmd == nil {
		return 0, errors.New("processgroup: nil command")
	}
	StartCommandInNewGroup(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return CommandGroupID(cmd), nil
}

// ReleaseCommand drops the launcher's ownership of cmd's tree. It is a no-op on
// Unix, where ownership is expressed by the group ID rather than by a handle.
func ReleaseCommand(_ *exec.Cmd) error {
	return nil
}

// ReleaseCommandIfEmpty drops ownership of cmd's tree once nothing is left in
// it. It is a no-op on Unix: the group ID stays addressable for as long as the
// group has members, with no handle to give up.
func ReleaseCommandIfEmpty(_ *exec.Cmd) error {
	return nil
}

// HoldsTree reports whether this package holds cmd's tree. It never does on
// Unix: a process group is addressed by its ID, and nothing is held.
func HoldsTree(_ *exec.Cmd) bool {
	return false
}

// CommandGroupID returns the process-group identifier that StartCommandInNewGroup
// established for cmd, or 0 if cmd was not started that way.
//
// It is derived from the spawn request rather than queried from the kernel, and
// that is the whole point. syscall.Getpgid answers only while the group LEADER
// is still a live, unreaped process; once the leader has exited and been waited
// on it fails with ESRCH even though surviving descendants still carry the group
// ID. Reading the ID from the request keeps it valid for exactly as long as the
// group can still contain something to kill.
func CommandGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil || cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		return 0
	}
	if cmd.SysProcAttr.Pgid != 0 {
		return cmd.SysProcAttr.Pgid
	}
	// Setpgid with Pgid 0 makes the child the leader of a new group whose ID
	// is its own PID.
	return cmd.Process.Pid
}

// resolveGroupID returns the group to act on for cmd, preferring an explicitly
// captured ID, then the ID implied by the spawn request, and only then a live
// kernel lookup for commands started outside this package.
func resolveGroupID(cmd *exec.Cmd, knownPGID int) (int, error) {
	if knownPGID > 0 {
		return knownPGID, nil
	}
	if pgid := CommandGroupID(cmd); pgid > 0 {
		return pgid, nil
	}
	resolved, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return 0, fmt.Errorf("resolve process group for pid %d: %w", cmd.Process.Pid, err)
	}
	return resolved, nil
}

// unsafeGroup reports whether pgid must never be signaled: init, the
// nonpositive values kill(2) reads as broadcast or current-group, and the
// caller's own group.
func unsafeGroup(pgid int, opts Options) bool {
	return pgid <= 1 || pgid == opts.currentGroupID()
}

// Terminate sends SIGTERM to pgid, waits for exit, then escalates to SIGKILL.
func Terminate(pgid int, timeout time.Duration, opts Options) error {
	if unsafeGroup(pgid, opts) {
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
	pgid, err := resolveGroupID(cmd, knownPGID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return killDirect(cmd.Process, err)
	}
	if err := Terminate(pgid, timeout, opts); err != nil {
		return killDirect(cmd.Process, fmt.Errorf("terminate process group %d: %w", pgid, err))
	}
	return nil
}

// KillCommandGroup delivers SIGKILL to cmd's whole process group immediately,
// with no graceful phase and no wait.
//
// This is the force path, kept separate from TerminateCommand so a caller that
// means "kill now" cannot be quietly upgraded to a graceful stop with a timeout.
// Failed-start cleanup uses it: the process never became usable, there is
// nothing to shut down politely, and the caller is about to return an error to
// someone waiting on it.
//
// ESRCH is success — the group is already gone.
func KillCommandGroup(cmd *exec.Cmd, knownPGID int, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := resolveGroupID(cmd, knownPGID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return killDirect(cmd.Process, err)
	}
	if unsafeGroup(pgid, opts) {
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := opts.kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return killDirect(cmd.Process, fmt.Errorf("kill process group %d: %w", pgid, err))
	}
	return nil
}

// InterruptCommand delivers an interrupt to cmd's whole process group: the
// request an interactive user makes with Ctrl+C, which the agent is expected to
// handle rather than die from.
func InterruptCommand(cmd *exec.Cmd, knownPGID int, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := resolveGroupID(cmd, knownPGID)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if unsafeGroup(pgid, opts) {
		return fmt.Errorf("refusing to signal unsafe process group %d", pgid)
	}
	if err := opts.kill(-pgid, syscall.SIGINT); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// TerminateManagedCommand runs the provider-managed stop sequence for an owned
// command: request a graceful stop, wait up to grace for the process to report
// its own exit on done, then force the tree down and wait for confirmation.
//
// done must be closed by whoever reaps cmd. Waiting on it rather than polling
// liveness is what makes the returned nil mean "observed exited" instead of
// "stopped looking".
func TerminateManagedCommand(cmd *exec.Cmd, knownPGID int, done <-chan struct{}, grace time.Duration, opts Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid, err := resolveGroupID(cmd, knownPGID)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	if pgid > 0 && !unsafeGroup(pgid, opts) {
		if termErr := opts.kill(-pgid, syscall.SIGTERM); termErr != nil && !errors.Is(termErr, syscall.ESRCH) {
			return fmt.Errorf("request stop of process group %d: %w", pgid, termErr)
		}
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
	}

	if err := KillCommandGroup(cmd, pgid, opts); err != nil {
		return err
	}
	<-done
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
