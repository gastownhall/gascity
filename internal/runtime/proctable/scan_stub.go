//go:build !linux && !darwin

package proctable

import (
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID is unavailable on platforms without process environment
// scanning support.
func ScanBySessionID(string) ([]runtime.LiveRuntime, error) {
	return []runtime.LiveRuntime{}, nil
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
