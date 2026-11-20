//go:build !windows

package config

import "testing"

// runShellCommandInProcess declines wherever a POSIX shell runs the command:
// runShellCommandCapture then hands it to `sh -c`.
func runShellCommandInProcess(*testing.T, string, []string) (stdout, stderr string, exit int, ok bool) {
	return "", "", 0, false
}
