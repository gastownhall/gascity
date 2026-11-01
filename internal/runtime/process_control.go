package runtime

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// ManagedProcessStopGrace is the shared grace period before escalating
// provider-managed process termination from SIGTERM to SIGKILL.
const ManagedProcessStopGrace = 5 * time.Second

// ManagedProcessReapGrace bounds how long a kill waits, after SIGKILL, for the
// target to actually leave the run/ready set before reporting it as
// not-confirmed-dead. A process wedged in uninterruptible sleep (D-state) under
// I/O can outlive its own SIGKILL until the I/O completes; waiting for
// confirmed death (gone or zombie) before starting a replacement is what keeps
// an escaped old process from racing the new one for the same work bead.
const ManagedProcessReapGrace = 3 * time.Second

// SignalProcessGroup sends sig to the managed process group when possible and
// falls back to the direct process signal for older sessions or platforms that
// cannot signal by group.
func SignalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}

// managedGroupPollInterval is how often TerminateManagedProcess checks whether
// a group whose leader has exited still has members.
const managedGroupPollInterval = 25 * time.Millisecond

// TerminateManagedProcess sends SIGTERM to the process group, waits up to
// grace for the whole group to exit, then SIGKILLs what is left of it.
//
// The leader exiting is not the group exiting. A member that handles or
// ignores SIGTERM outlives a leader that does not (a wrapper shell dies at
// once while the program it started finishes what it was doing, or never
// stops), and nothing would kill it afterwards: for a provider whose control
// socket lives in the stopping process, nothing can even reach it.
func TerminateManagedProcess(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) error {
	_ = SignalProcessGroup(cmd, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		if !startedAsGroupLeader(cmd) || groupGoneBefore(cmd.Process.Pid, timer.C) {
			return nil
		}
	case <-timer.C:
	}

	_ = SignalProcessGroup(cmd, syscall.SIGKILL)
	<-done
	return nil
}

// startedAsGroupLeader reports whether cmd was started as the leader of a new
// process group, so its PID names that group even after the leader is reaped.
func startedAsGroupLeader(cmd *exec.Cmd) bool {
	return cmd != nil && cmd.Process != nil && cmd.SysProcAttr != nil &&
		cmd.SysProcAttr.Setpgid && cmd.SysProcAttr.Pgid == 0
}

// groupGoneBefore polls process group pgid until it has no members (true) or
// expired fires (false). A member this process may not signal still counts.
func groupGoneBefore(pgid int, expired <-chan time.Time) bool {
	ticker := time.NewTicker(managedGroupPollInterval)
	defer ticker.Stop()
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		select {
		case <-expired:
			return false
		case <-ticker.C:
		}
	}
}
