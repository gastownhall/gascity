//go:build !linux && !darwin

package proctable

import "fmt"

// KillByPID is unavailable on platforms without process signaling support.
func KillByPID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	return fmt.Errorf("proctable: KillByPID is unsupported on this platform")
}
