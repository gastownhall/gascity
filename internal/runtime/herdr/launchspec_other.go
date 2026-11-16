//go:build !windows

package herdr

import "path/filepath"

// launchSyntaxPortable is true here: the POSIX splitting that finds the program
// and its arguments is the shell's own syntax, and launchShellMetachars already
// names what it cannot be trusted with.
func launchSyntaxPortable(string) bool { return true }

// agentKindOf names the agent kind program would be: its basename, exactly.
func agentKindOf(program string) string {
	return filepath.Base(program)
}
