//go:build !linux && !darwin && !windows

package proctable

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionIDOutsideTmux is unavailable where ScanBySessionID is: it
// returns ScanBySessionID's answer, an error.
func ScanBySessionIDOutsideTmux(id string) ([]runtime.LiveRuntime, error) {
	return ScanBySessionID(id)
}

// FenceTmuxSubtrees cannot read an ancestry on this platform, so it keeps
// no root and reports why.
func FenceTmuxSubtrees(found []runtime.LiveRuntime) ([]runtime.LiveRuntime, error) {
	if len(found) == 0 {
		return []runtime.LiveRuntime{}, nil
	}
	return []runtime.LiveRuntime{}, fmt.Errorf("proctable: process ancestry is unavailable: %w", ErrUnsupported)
}
