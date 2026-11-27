//go:build linux

package main

import "os"

// fsPressurePath is the default Linux PSI (Pressure Stall Information) path
// for filesystem/block IO pressure. It is a var so tests can inject a fake.
var fsPressurePath = "/proc/pressure/io"

// fsPressureReadFile is the file reader used to load the pressure file. It is
// a var so tests can inject a fake reader without touching the real /proc.
var fsPressureReadFile = os.ReadFile

// readFSPressureAvg60 returns the "some avg60" value from the PSI IO file at
// path. On non-Linux builds this is stubbed to return 0. On Linux, parse
// errors (file missing, malformed contents, unparseable number) are returned
// as errors so the caller can decide how to treat them — the gate treats any
// error as "proceed normally" to fail open.
func readFSPressureAvg60(path string) (float64, error) {
	return readPSIAvg(fsPressureReadFile, path, "avg60")
}
