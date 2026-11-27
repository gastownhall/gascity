package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// defaultHostPressureThreshold is the CPU "some avg10" above which the
// controller holds starts and creates: tasks stalled on CPU more than half of
// the last ten seconds.
const defaultHostPressureThreshold = 50.0

// hostPressureThresholdEnv overrides defaultHostPressureThreshold.
const hostPressureThresholdEnv = "GC_SUPERVISOR_CPU_PRESSURE_THRESHOLD"

// hostPressurePath is the Linux PSI file for CPU pressure.
var hostPressurePath = "/proc/pressure/cpu"

// hostPressureReadFile loads the CPU PSI file. It is a var so tests can serve
// a fake without touching the real /proc.
var hostPressureReadFile = os.ReadFile

// hostPressure is one reading of host CPU pressure against the threshold.
type hostPressure struct {
	Avg10     float64
	Threshold float64
	High      bool
}

// readHostPressure reads the CPU PSI "some avg10" and compares it with the
// threshold. High means avg10 is above the threshold, so a threshold of 100
// never holds. A file that cannot be read or parsed reads as not high, so
// the hold fails open.
func readHostPressure(_ io.Writer) hostPressure {
	threshold := hostPressureThreshold()
	avg10, err := readPSIAvg(hostPressureReadFile, hostPressurePath, "avg10")
	if err != nil {
		return hostPressure{Threshold: threshold}
	}
	return hostPressure{Avg10: avg10, Threshold: threshold, High: avg10 > threshold}
}

// hostPressureThreshold is the configured CPU pressure threshold, in [0,100];
// an unset or invalid value is the default.
func hostPressureThreshold() float64 {
	raw := os.Getenv(hostPressureThresholdEnv)
	if raw == "" {
		return defaultHostPressureThreshold
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || v < 0 || v > 100 {
		return defaultHostPressureThreshold
	}
	return v
}

// readPSIAvg returns the window field ("avg10", "avg60", "avg300") of the
// "some" line of the PSI file at path, read through read. A missing file, a
// missing line or field, or an unparseable number is an error.
func readPSIAvg(read func(string) ([]byte, error), path, window string) (float64, error) {
	data, err := read(path)
	if err != nil {
		return 0, err
	}
	// PSI format:
	//   some avg10=0.00 avg60=0.00 avg300=0.00 total=0
	//   full avg10=0.00 avg60=0.00 avg300=0.00 total=0
	prefix := window + "="
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, prefix) {
				continue
			}
			v, perr := strconv.ParseFloat(strings.TrimPrefix(field, prefix), 64)
			if perr != nil {
				return 0, fmt.Errorf("parse %s: %w", window, perr)
			}
			return v, nil
		}
		return 0, fmt.Errorf("%s field not found on 'some' line", window)
	}
	return 0, fmt.Errorf("'some' line not found in %s", path)
}
