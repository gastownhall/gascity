//go:build !linux && !darwin && !windows

package proctable

import "fmt"

// rootArgv is unavailable on platforms without process table access; the
// scanner reports no roots there either.
func rootArgv(pid int) ([]string, error) {
	return nil, fmt.Errorf("proctable: argv for PID %d: %w", pid, ErrUnsupported)
}

func rootStartIdentity(int) string { return "" }
