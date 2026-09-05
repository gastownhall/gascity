//go:build !windows

package execgrace

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// interruptRetryInterval and interruptRetryBudget bound how long
// interruptProcessGroup keeps resending SIGINT to the command's process
// group after the first delivery. They exist to close a fork race, not to
// wait out a slow adapter: a genuinely cooperating command runs its trap in
// milliseconds, so a budget far inside a typical WaitDelay grace period is
// enough to catch a foreground child that was mid-fork at the first attempt
// without meaningfully delaying the case where nothing is missed.
const (
	interruptRetryInterval = 10 * time.Millisecond
	interruptRetryBudget   = 300 * time.Millisecond
)

// setProcessGroup puts the command in its own process group so a cooperative
// cancellation can be delivered to the whole group — reaching any foreground
// child (for example a long-running git checkout under a setup shell) that
// would otherwise keep the shell from running its rollback trap before the
// forced kill.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// interruptProcessGroup sends os.Interrupt to the command's process group so a
// foreground child receives it alongside the shell leader. It preserves the
// os.ErrProcessDone signal the caller special-cases: a target that was
// already gone before ANY delivery reports ErrProcessDone rather than a
// spurious failure. If the group id cannot be resolved it falls back to
// signaling the leader directly.
//
// The first delivery can race a foreground child the command is still
// fork()ing (e.g. a readiness sleep in a setup script): if the child is not
// yet a member of the process group at the instant of the kill(2) call, only
// the shell leader receives the signal. A POSIX shell defers running its trap
// while waiting on a foreground child, so once that child does appear moments
// later the pending rollback cannot run until it exits naturally — starving
// past WaitDelay and losing cleanup entirely. Resending the signal for a
// short budget closes that race: any retry issued after the child has
// actually joined the group reaches it, without widening WaitDelay itself or
// changing behavior for the (overwhelmingly common) case where nothing was
// missed.
//
// Once at least one delivery has succeeded, a later ESRCH (the whole group
// has since exited — typically the command's trap already ran and returned)
// is reported as nil, not ErrProcessDone: [InterruptThenKill]'s caller treats
// a nil Cancel() as "cancellation was delivered", and the [os/exec] package
// only substitutes ctx.Err() for the process's own exit status on that nil
// return (see Cmd.watchCtx in the standard library). Returning ErrProcessDone
// here for a group that answered our own earlier signal would make that
// substitution not happen, and a fast, successful rollback would then be
// misreported as an ordinary clean exit instead of a cancellation.
func interruptProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		return cmd.Process.Signal(os.Interrupt)
	}

	delivered := false
	deadline := time.Now().Add(interruptRetryBudget)
	for {
		killErr := syscall.Kill(-pgid, syscall.SIGINT)
		if killErr != nil {
			if errors.Is(killErr, syscall.ESRCH) {
				if delivered {
					return nil
				}
				return os.ErrProcessDone
			}
			return killErr
		}
		delivered = true
		// A dead leader means delivery took effect; signal(0) here is just a
		// liveness probe, not another send.
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return nil
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(interruptRetryInterval)
	}
}
