//go:build !linux && !darwin

package proctable

import "fmt"

// ProcessIdentity is unavailable on platforms without a safe process start
// identity reader.
func ProcessIdentity(pid int) (string, error) {
	return "", fmt.Errorf("process identity for PID %d is unsupported on this platform", pid)
}
