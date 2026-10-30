//go:build !windows

package subprocess

import "github.com/gastownhall/gascity/internal/runtime"

// isUnavailableSocketError reports whether err means no session is listening
// at the pathname, rather than a fault the caller has to surface.
func isUnavailableSocketError(err error) bool {
	return err != nil && runtime.ClassifyControlSocketDial(err) == runtime.ControlSocketAbsent
}
