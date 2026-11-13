//go:build !linux && !darwin && !windows

package proctable

import (
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID is unavailable on platforms without process environment
// scanning support. The error is the whole answer: callers gating a start on
// "no orphan is running" must refuse, because an unavailable scan cannot
// distinguish a clean host from one with survivors.
func ScanBySessionID(string) ([]runtime.LiveRuntime, error) {
	return nil, fmt.Errorf("proctable: process scanning is unavailable: %w", ErrUnsupported)
}

// ScanBySessionIDSince is unavailable on platforms without process
// environment scanning support. A bounded scan — a non-zero
// incarnationStartedAt — fails closed with ErrIncarnationBoundUnsupported:
// this platform cannot look at all, so the empty inventory must not read as a
// complete one. The unbounded scan keeps returning an empty inventory.
func ScanBySessionIDSince(_ string, incarnationStartedAt time.Time) ([]runtime.LiveRuntime, error) {
	if !incarnationStartedAt.IsZero() {
		return []runtime.LiveRuntime{}, ErrIncarnationBoundUnsupported
	}
	return []runtime.LiveRuntime{}, nil
}

// IsScanRoot reports false on platforms without process environment scanning
// support.
func IsScanRoot(int) bool {
	return false
}
