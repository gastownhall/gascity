//go:build !linux && !darwin && !windows

package proctable

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID is unavailable on platforms without process environment
// scanning support. The error is the whole answer: callers gating a start on
// "no orphan is running" must refuse, because an unavailable scan cannot
// distinguish a clean host from one with survivors.
func ScanBySessionID(string) ([]runtime.LiveRuntime, error) {
	return nil, fmt.Errorf("proctable: process scanning is unavailable: %w", ErrUnsupported)
}

// IsScanRoot reports false on platforms without process environment scanning
// support.
func IsScanRoot(int) bool {
	return false
}
