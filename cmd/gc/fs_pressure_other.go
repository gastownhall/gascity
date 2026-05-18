//go:build !linux

package main

// fsPressurePath is unused on non-Linux but declared so shared code can
// reference it in tests without build-tag juggling.
var fsPressurePath = ""

// fsPressureReadFile is unused on non-Linux but declared so tests on
// non-Linux platforms can still reference the symbol.
var fsPressureReadFile = func(string) ([]byte, error) { return nil, nil }

// readFSPressureAvg60 always returns 0 on non-Linux so the backpressure gate
// is a no-op. Linux is the only platform that exposes PSI at /proc/pressure.
func readFSPressureAvg60(_ string) (float64, error) {
	return 0, nil
}
