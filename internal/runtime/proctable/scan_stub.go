//go:build !linux && !darwin

package proctable

import "github.com/gastownhall/gascity/internal/runtime"

// ScanBySessionID is unavailable on platforms without process environment
// scanning support.
func ScanBySessionID(string) ([]runtime.LiveRuntime, error) {
	return []runtime.LiveRuntime{}, nil
}

// IsScanRoot reports false on platforms without process environment scanning
// support.
func IsScanRoot(int) bool {
	return false
}
