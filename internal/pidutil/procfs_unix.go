//go:build !windows

package pidutil

import (
	"os"
	"path/filepath"
	"strconv"
)

// procPath builds /proc/<pid>/<elem...>.
func procPath(pid int, elem ...string) string {
	parts := make([]string, 0, len(elem)+2)
	parts = append(parts, "/proc", strconv.Itoa(pid))
	parts = append(parts, elem...)
	return filepath.Join(parts...)
}

// readProcFile reads /proc/<pid>/<elem...>. On a host without procfs (darwin)
// the read fails with the ordinary filesystem error and callers fall back to
// their platform mechanism.
func readProcFile(pid int, elem ...string) ([]byte, error) {
	return os.ReadFile(procPath(pid, elem...))
}

// readProcDir lists /proc/<pid>/<elem...>.
func readProcDir(pid int, elem ...string) ([]os.DirEntry, error) {
	return os.ReadDir(procPath(pid, elem...))
}
