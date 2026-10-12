//go:build linux

package proctable

import (
	"path/filepath"
	"strconv"
)

// ProcessEnvValue returns the value of key in pid's environment, or "" when
// the process is gone, its environment is unreadable, or key is absent. It
// reads the same procfs root as [ScanBySessionID] and refuses the live /proc
// under go test for the same reason.
func ProcessEnvValue(pid int, key string) (string, error) {
	if err := liveScanGuard(); err != nil {
		return "", err
	}
	env, err := parseEnvironFile(filepath.Join(scanRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", err
	}
	return env[key], nil
}
