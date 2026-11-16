//go:build !windows

package herdr

import "strings"

// rawLaunchTyped reports whether the fake's call log shows a raw launch typed
// into pane %5 the way launchRaw types one on this platform.
func rawLaunchTyped(calls string) bool {
	return strings.Contains(calls, "pane run %5 exec /bin/sh -c ")
}
