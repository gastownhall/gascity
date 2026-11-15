//go:build !linux && !darwin && !windows

package proctable

import "fmt"

// ProcessIdentity is unavailable on platforms without a safe process start
// identity reader.
func ProcessIdentity(pid int) (string, error) {
	return "", fmt.Errorf("proctable: process identity for PID %d: %w", pid, ErrUnsupported)
}
