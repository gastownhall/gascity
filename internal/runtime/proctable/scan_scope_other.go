//go:build !windows

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanBySessionIDForExecutables uses the ordinary scanner on platforms without
// Windows process-inspection failures.
func ScanBySessionIDForExecutables(id string, _ []string) ([]runtime.LiveRuntime, error) {
	return ScanBySessionID(id)
}
