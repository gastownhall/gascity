//go:build windows

package runtime

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// ManagedProcessStopGrace is the shared grace period before escalating
// provider-managed process termination from requested stop to force.
const ManagedProcessStopGrace = 5 * time.Second

// ManagedProcessReapGrace bounds how long a force kill waits for confirmation.
const ManagedProcessReapGrace = 3 * time.Second

// SignalProcessGroup is intentionally unavailable on Windows: Windows has no
// SIGTERM/SIGKILL process-group analogue, and callers that need graceful stop
// must use a provider/control protocol instead of signal emulation.
//
// See internal/processgroup for the supported path: InterruptCommand delivers a
// console control event to an owned group, and TerminateManagedCommand reaps an
// owned Job Object.
func SignalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return fmt.Errorf("managed process signal %v is unsupported on Windows", sig)
}

// TerminateManagedProcess cannot reap a managed process tree on Windows.
//
// A Windows process has no containment relationship with the processes it
// creates: killing it leaves its descendants running, and this package is
// pinned to the standard library (see privatedir.go), so it cannot reach the
// Job Object that owns the tree. It therefore does the part it can — waiting
// out the grace period and killing the direct process — and reports the part it
// cannot, instead of returning nil and letting a caller record a stop that did
// not happen.
//
// Callers that own their child should use processgroup.TerminateManagedCommand,
// which terminates the owning Job Object and confirms the tree is empty.
func TerminateManagedProcess(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) error {
	timer := time.NewTimer(grace)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
	}

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	incomplete := fmt.Errorf("descendants of pid %d cannot be reaped through this entry point on Windows; use an owned process group", cmd.Process.Pid)
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return errors.Join(incomplete, err)
	}
	<-done
	return incomplete
}
