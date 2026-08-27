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
// environment scanning support.
func ScanBySessionIDSince(string, time.Time) ([]runtime.LiveRuntime, error) {
	return []runtime.LiveRuntime{}, nil
}

// ScanBySessionIDSinceInScope is unavailable on platforms without process
// environment scanning support.
func ScanBySessionIDSinceInScope(string, time.Time, SessionScope) ([]runtime.LiveRuntime, error) {
	return []runtime.LiveRuntime{}, nil
}

// IsScanRoot reports false on platforms without process environment scanning
// support.
func IsScanRoot(int) bool {
	return false
}
