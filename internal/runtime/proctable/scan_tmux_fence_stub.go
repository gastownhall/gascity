//go:build !linux && !darwin && !windows

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanBySessionIDOutsideTmux is unavailable where ScanBySessionID is: it
// returns ScanBySessionID's answer, an error.
func ScanBySessionIDOutsideTmux(id string) ([]runtime.LiveRuntime, error) {
	return ScanBySessionID(id)
}
