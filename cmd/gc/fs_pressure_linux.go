//go:build linux

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

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
	data, err := fsPressureReadFile(path)
	if err != nil {
		return 0, err
	}
	// PSI format example:
	//   some avg10=0.00 avg60=0.00 avg300=0.00 total=0
	//   full avg10=0.00 avg60=0.00 avg300=0.00 total=0
	// We want the avg60 on the "some" line.
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if !strings.HasPrefix(field, "avg60=") {
				continue
			}
			v, perr := strconv.ParseFloat(strings.TrimPrefix(field, "avg60="), 64)
			if perr != nil {
				return 0, fmt.Errorf("parse avg60: %w", perr)
			}
			return v, nil
		}
		return 0, fmt.Errorf("avg60 field not found on 'some' line")
	}
	return 0, fmt.Errorf("'some' line not found in %s", path)
}
