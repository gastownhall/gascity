//go:build !linux

package main

// fsPressurePath is unused on non-Linux but declared so shared code can
// reference it in tests without build-tag juggling.
var fsPressurePath = ""

// fsPressureReadFile mirrors the Linux declaration so the shared test helper
// configureFSPressureForTests (main_test.go) compiles on all platforms. The
// non-Linux readFSPressureAvg60 stub never reads it, so it is write-only here;
// silence the unused check rather than build-tag-splitting the test helper.
//
//nolint:unused // assigned cross-platform by tests; only read on Linux
var fsPressureReadFile = func(string) ([]byte, error) { return nil, nil }

// readFSPressureAvg60 always returns 0 on non-Linux so the backpressure gate
// is a no-op. Linux is the only platform that exposes PSI at /proc/pressure.
func readFSPressureAvg60(_ string) (float64, error) {
	return 0, nil
}
