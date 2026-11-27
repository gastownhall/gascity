package beadmeta

import (
	"strings"
	"time"
)

// FormatRoutedAt renders t as the gc.routed_at value (RoutedAtMetadataKey):
// UTC RFC3339.
//
// gc.routed_at records when a built-in router last changed a bead's
// gc.routed_to. It is a timestamp about a route, not itself a route: the
// orphan-release sweep reads it as the age of a handoff away from a live
// holder, and no reader treats it as a routing target.
func FormatRoutedAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// ParseRoutedAt parses a gc.routed_at value. It trims v and reports false for
// an empty or malformed value, which callers must treat as "no stamp".
func ParseRoutedAt(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
