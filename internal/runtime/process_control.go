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

// processGroupKill sends sig to pid. A negative pid selects the process group
// whose id is -pid. Tests replace it to observe which target was chosen.
var processGroupKill = syscall.Kill

// SignalProcessGroup signals the process group recorded at spawn when
// knownPGID is set, and otherwise signals cmd's process directly.
//
// knownPGID is the leader pid captured after a successful Setpgid start. That
// id still names the group after the leader has been reaped. Getpgid cannot
// see that state: it returns ESRCH while kill of the recorded group still
// reaches descendants. Callers that did not create a group pass 0 so the
// signal never widens to a group this session does not own.
func SignalProcessGroup(cmd *exec.Cmd, knownPGID int, sig syscall.Signal) error {
	if knownPGID > 1 {
		return processGroupKill(-knownPGID, sig)
	}
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 1 {
		return nil
	}
	return processGroupKill(cmd.Process.Pid, sig)
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
func TerminateManagedProcess(cmd *exec.Cmd, knownPGID int, done <-chan struct{}, grace time.Duration) error {
	_ = SignalProcessGroup(cmd, knownPGID, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		if knownPGID <= 1 || groupGoneBefore(knownPGID, timer.C) {
			return nil
		}
	case <-timer.C:
	}

	_ = SignalProcessGroup(cmd, knownPGID, syscall.SIGKILL)
	<-done
	return nil
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
