package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanAll returns all live agent root processes with a non-empty
// GC_SESSION_ID.
func ScanAll() ([]runtime.LiveRuntime, error) {
	return ScanBySessionID("")
}
