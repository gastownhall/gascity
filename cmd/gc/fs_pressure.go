package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
)

// Default threshold for "some avg60" before we skip a supervisor tick. A
// value of 50.0 means that if tasks were stalled on IO more than 50% of the
// last 60 seconds, the tick is skipped to avoid piling on more writes.
const defaultFSPressureThreshold = 50.0

// fsPressureThresholdEnv is the env var that overrides defaultFSPressureThreshold.
const fsPressureThresholdEnv = "GC_SUPERVISOR_FS_PRESSURE_THRESHOLD"

// fsPressureThreshold returns the currently configured IO pressure threshold.
// Invalid env values fall back to the default.
func fsPressureThreshold() float64 {
	raw := os.Getenv(fsPressureThresholdEnv)
	if raw == "" {
		return defaultFSPressureThreshold
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return defaultFSPressureThreshold
	}
	return v
}

// shouldSkipTickForFSPressure returns true when the current "some avg60" IO
// pressure exceeds the configured threshold. On error (missing file, parse
// failure, non-Linux) it fails open — i.e. returns false so the tick proceeds
// normally. When skipping, a single warning is written to stderr.
//
// The caller (the supervisor tick) should short-circuit any expensive work
// (buildDesiredState, spawning, reconciliation) when this returns true.
func shouldSkipTickForFSPressure(stderr io.Writer) bool {
	threshold := fsPressureThreshold()
	avg60, err := readFSPressureAvg60(fsPressurePath)
	if err != nil {
		// Fail open: if we can't read PSI, don't block work.
		return false
	}
	if avg60 > threshold {
		if stderr != nil {
			fmt.Fprintf(stderr, "supervisor: FS pressure high (some avg60=%.2f > threshold=%.1f), skipping tick\n", //nolint:errcheck // best-effort stderr
				avg60, threshold)
		}
		return true
	}
	return false
}
