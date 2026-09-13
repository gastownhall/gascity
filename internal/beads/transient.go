package beads

import "strings"

var transientConnErrorMarkers = [...]string{
	"i/o timeout",
	"invalid connection",
	"bad connection",
	"connection reset",
	"broken pipe",
	"timed out after",
	"deadline exceeded",
}

// IsTransientConnError reports whether err looks like a transient connection
// failure from bd, Dolt, or the transport layers beneath them.
func IsTransientConnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transientConnErrorMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
