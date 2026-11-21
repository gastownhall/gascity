package beadmeta

import (
	"strconv"
	"strings"
)

// GCExempt reports whether meta stamps GCExemptMetadataKey true: any value
// strconv.ParseBool reads as true, or "yes" or "on" in any case, surrounding
// space ignored. A deployment stamps it on a root no garbage collector may
// close (the wisp GC's abandoned-root closer, the failed-molecule sweep); the
// SDK stamps it on no root of its own.
func GCExempt(meta map[string]string) bool {
	v := strings.TrimSpace(meta[GCExemptMetadataKey])
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	switch strings.ToLower(v) {
	case "yes", "on":
		return true
	default:
		return false
	}
}
