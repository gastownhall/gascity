//go:build windows

package proctable

import "strings"

// processNameKey is the form process names are compared in. Windows file names
// are case-insensitive, and a process snapshot names each process by its image
// file, ".exe" included, while configurations name them the Unix way ("gc",
// "claude"). Both sides are case-folded and the extension dropped, so
// "CLAUDE.EXE" in a snapshot matches "claude" in a config.
func processNameKey(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".exe")
}
