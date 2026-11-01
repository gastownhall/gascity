//go:build !linux && !darwin

package proctable

import "fmt"

// OpenFiles returns nothing on platforms without open-file scanning support.
func OpenFiles(int) ([]string, error) {
	return nil, nil
}

// ProcessCwd reports that this platform has no support for reading a
// process's working directory: an error, never a directory.
func ProcessCwd(pid int) (string, error) {
	return "", fmt.Errorf("reading the working directory of pid %d: not supported on this platform", pid)
}
