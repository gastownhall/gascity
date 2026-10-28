//go:build !windows

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
