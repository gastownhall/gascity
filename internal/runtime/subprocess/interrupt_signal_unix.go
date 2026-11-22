//go:build !windows

package subprocess

import (
	"syscall"

	"github.com/gastownhall/gascity/internal/runtime"
)

// signalInterrupt sends SIGINT to sc's process group.
func signalInterrupt(sc *sessionConn) error {
	return runtime.SignalProcessGroup(sc.cmd, syscall.SIGINT)
}
