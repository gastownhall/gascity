//go:build !linux && !darwin && !windows

package proctable

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// KillByPID is unavailable on platforms without process signaling support.
func KillByPID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	return fmt.Errorf("proctable: KillByPID for PID %d: %w", pid, ErrUnsupported)
}

// KillRuntime is unavailable on platforms without process signaling support.
func KillRuntime(live runtime.LiveRuntime) error {
	return KillByPID(live.PID)
}

// RescanAfterTermination is false here: nothing is ever terminated.
const RescanAfterTermination = false
