//go:build windows

package tmuxtest

import (
	"fmt"
	"syscall"
)

// signalProcess reports that a bare PID cannot be signalled on Windows.
//
// There is no signal delivery mechanism a PID alone can address, and the
// number may already have been recycled onto an unrelated process, so the
// escalation this backs is unavailable rather than silently successful. The
// sweep that calls it only reaps servers found behind Unix domain socket
// files, which do not occur on Windows, so the error is a contract statement
// rather than a path taken.
func signalProcess(pid int, sig syscall.Signal) error {
	return fmt.Errorf("tmuxtest: signalling pid %d with %v is unsupported on windows", pid, sig)
}
