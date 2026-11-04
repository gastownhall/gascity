//go:build !linux

package tmux

import "errors"

// paneSubtreeSupported reports that readPaneSubtree can read a pane's
// process tree on this platform: there is no /proc here.
const paneSubtreeSupported = false

// readPaneSubtree is unavailable without /proc; the pane probes take the full
// snapshot instead.
func readPaneSubtree(string) (processSnapshot, error) {
	return processSnapshot{}, errors.New("reading a process subtree: no /proc on this platform")
}
