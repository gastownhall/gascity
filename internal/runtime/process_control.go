package runtime

import (
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

// signalKill is syscall.Kill behind an indirection so tests can observe
// whether a signal was aimed at a process group or a single process.
var signalKill = syscall.Kill

// leadsOwnGroup reports whether pid leads the process group its own number
// names — the only condition under which kill(-pid, sig) stays scoped to
// pid's own tree.
//
// This duplicates processgroup.LeadsOwnGroup on purpose: the root
// internal/runtime package is the in-process expression of the Runtime
// Provider Protocol contract and is pinned to standard-library imports by
// TestRuntimeContractPackageStaysStdlibOnly, so it cannot import the helper
// package. Keep the two in sync.
func leadsOwnGroup(pid int) bool {
	if pid <= 1 {
		return false
	}
	pgid, err := syscall.Getpgid(pid)
	return err == nil && pgid == pid
}

// SignalProcessGroup sends sig to the managed process group when possible and
// falls back to the direct process signal for older sessions or platforms that
// cannot signal by group.
//
// Group leadership is confirmed before widening the signal instead of being
// inferred from a failed group kill. kill(-pid) selects the group whose id
// equals pid, not "the group containing pid", so for a managed child that
// never became a group leader it names an unrelated tree that happens to hold
// that number — and succeeds, which makes the fallback below unreachable in
// exactly the case it was written for. On a host running many agents that
// lands as one process tree dying to a signal nobody sent it (ga-8qmy).
func SignalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if leadsOwnGroup(cmd.Process.Pid) {
		if err := signalKill(-cmd.Process.Pid, sig); err == nil {
			return nil
		}
	}
	return cmd.Process.Signal(sig)
}

// TerminateManagedProcess sends SIGTERM, waits for done, then escalates to
// SIGKILL after grace if the process group is still alive.
func TerminateManagedProcess(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) error {
	_ = SignalProcessGroup(cmd, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
	}

	_ = SignalProcessGroup(cmd, syscall.SIGKILL)
	<-done
	return nil
}
