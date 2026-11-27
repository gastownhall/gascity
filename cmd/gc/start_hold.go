package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// Start-hold causes: why a pass holds every start and create. The empty cause
// holds nothing.
const (
	// causeSuspensionUnreadable holds starts while the suspension file exists
	// but does not parse: the operator's brake cannot be read, so nothing
	// wakes until it can.
	causeSuspensionUnreadable = "suspension-unreadable"
	// causeHostPressure holds starts while host CPU pressure is above the
	// threshold, so a loaded host is not loaded further.
	causeHostPressure = "host-pressure"
)

// startHold is a pass's controller-wide hold on starts and creates. Cause is
// the deferral cause of every held start and create; Trickle lets the pass
// admit at most one of them through the hold.
type startHold struct {
	Cause   string
	Trickle bool
}

// held reports whether the hold defers anything.
func (h startHold) held() bool { return h.Cause != "" }

// startHoldInput is everything decideStartHold reads.
type startHoldInput struct {
	// SuspensionErr is the pass's wake read error of the suspension file.
	SuspensionErr error
	// HostPressure is the pass's CPU pressure reading.
	HostPressure hostPressure
}

// decideStartHold is the pass's start hold. An unreadable suspension file
// holds every start and create and never trickles; it wins over high CPU
// pressure, which holds them too.
func decideStartHold(in startHoldInput) startHold {
	switch {
	case in.SuspensionErr != nil:
		return startHold{Cause: causeSuspensionUnreadable}
	case in.HostPressure.High:
		return startHold{Cause: causeHostPressure}
	}
	return startHold{}
}

// suspensionWakeRemedy is what an operator does about an unreadable
// suspension file.
const suspensionWakeRemedy = "holding every start until it parses (fix the JSON, remove the file, or run `gc suspend`)"

// wakeSuspension is the controller's read of the suspension file for a pass
// that decides starts (readSuspensionForWake). An unreadable file prints one
// stderr line per distinct error, naming the file, the error and the remedy.
func (cr *CityRuntime) wakeSuspension() (suspensionstate.State, error) {
	st, err := readSuspensionForWake(cr.cityPath)
	cr.suspensionWakeMu.Lock()
	defer cr.suspensionWakeMu.Unlock()
	if err == nil {
		cr.suspensionWakeLogged = ""
		return st, nil
	}
	if msg := err.Error(); msg != cr.suspensionWakeLogged {
		cr.suspensionWakeLogged = msg
		fmt.Fprintf(cr.stderr, "gc: suspension file unreadable: %v; %s\n", err, suspensionWakeRemedy) //nolint:errcheck // best-effort stderr
	}
	return st, err
}

// startHold is the legacy tick's start hold over a fresh wake read and CPU
// pressure reading.
func (cr *CityRuntime) startHold() startHold {
	_, err := cr.wakeSuspension()
	return decideStartHold(startHoldInput{SuspensionErr: err, HostPressure: readHostPressure(cr.stderr)})
}
