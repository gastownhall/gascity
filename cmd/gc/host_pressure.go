package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
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

// maxHostPressureHoldIntervals is how many patrol intervals a CPU-pressure
// hold lasts, from the episode's start or the last forced start, before the
// next pass forces one start or create through it, so sustained pressure
// slows wakes without stopping them.
const maxHostPressureHoldIntervals = 5

// The outcomes of a supervisor.cpu_pressure.held_starts event.
const (
	hostPressureOutcomeHeld     = "held"
	hostPressureOutcomeForced   = "forced"
	hostPressureOutcomeReleased = "released"
)

// hostPressureEpisode is a reconciler's memory of a CPU-pressure episode:
// whether one is under way and when its trickle window opened.
type hostPressureEpisode struct {
	active    bool
	holdSince time.Time
}

// observe takes a pass's pressure reading at now. It reports whether the
// pass may force one start or create through the hold (the window has lasted
// maxHostPressureHoldIntervals patrol intervals) and the event outcome the
// reading brings: held when an episode begins, released when it ends, ""
// otherwise.
func (e *hostPressureEpisode) observe(p hostPressure, now time.Time, patrol time.Duration) (trickle bool, outcome string) {
	switch {
	case !p.High && !e.active:
		return false, ""
	case !p.High:
		*e = hostPressureEpisode{}
		return false, hostPressureOutcomeReleased
	case !e.active:
		*e = hostPressureEpisode{active: true, holdSince: now}
		return false, hostPressureOutcomeHeld
	}
	return now.Sub(e.holdSince) >= maxHostPressureHoldIntervals*patrol, ""
}

// forced restarts the trickle window at now, after a pass forced a start or
// create through the hold.
func (e *hostPressureEpisode) forced(now time.Time) { e.holdSince = now }

// noteHostPressure reports a held_starts outcome: the event, and for held the
// episode's one stderr line. An empty outcome reports nothing.
func noteHostPressure(rec events.Recorder, stderr io.Writer, cityName string, p hostPressure, outcome string) {
	if outcome == "" {
		return
	}
	if outcome == hostPressureOutcomeHeld && stderr != nil {
		fmt.Fprintf(stderr, "gc: host CPU pressure high (some avg10=%.2f > threshold=%.1f); holding starts and creates, forcing one through every %d patrol intervals\n", //nolint:errcheck // best-effort stderr
			p.Avg10, p.Threshold, maxHostPressureHoldIntervals)
	}
	if rec == nil {
		return
	}
	raw, err := json.Marshal(events.SupervisorCPUPressureHeldStartsPayload{
		Avg10: p.Avg10, Threshold: p.Threshold, MaxHoldIntervals: maxHostPressureHoldIntervals, Outcome: outcome,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc: encoding %s payload: %v\n", events.SupervisorCPUPressureHeldStarts, err) //nolint:errcheck // best-effort stderr
		return
	}
	rec.Record(events.Event{
		Type:    events.SupervisorCPUPressureHeldStarts,
		Actor:   "supervisor",
		Subject: cityName,
		Message: "host CPU pressure: " + outcome,
		Payload: raw,
	})
}
