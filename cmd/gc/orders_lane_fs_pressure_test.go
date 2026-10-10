//go:build linux

package main

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The FS-pressure gate still suppresses order dispatch now that dispatch runs
// on its own lane: a pass under high pressure writes no tracking and reaches
// neither the managed-Dolt preflight nor the dispatcher, and — like the tick —
// sheds at most maxConsecutiveFSPressureSkips passes before forcing one through
// so orders cannot starve under sustained external IO pressure.
func TestOrdersLanePassSkipsDispatchUnderFSPressure(t *testing.T) {
	od := &recordingOrderDispatcher{}
	var stderr bytes.Buffer
	cr := ordersLaneTestRuntime(t, od, "1h", &stderr)
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	cr.managedDoltOwned = func(string) (bool, error) {
		t.Fatal("managed dolt preflight should not run before the pressure gate")
		return false, nil
	}

	for i := 0; i < maxConsecutiveFSPressureSkips; i++ {
		cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	}
	if od.called.Load() {
		t.Fatalf("order dispatch ran during %d pressure-skipped lane passes", maxConsecutiveFSPressureSkips)
	}
	out := stderr.String()
	if !strings.Contains(out, "FS pressure high") || !strings.Contains(out, "skipping order dispatch") {
		t.Fatalf("stderr = %q, want an order-dispatch FS pressure skip warning", out)
	}
	if n := strings.Count(out, "skipping order dispatch"); n != 1 {
		t.Fatalf("skip warnings = %d, want 1 per pressure episode", n)
	}

	// The next pass is forced through, as the tick's gate would.
	cr.managedDoltOwned = func(string) (bool, error) { return false, nil }
	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	if !od.called.Load() {
		t.Fatal("order dispatch did not run on the forced pass after max consecutive pressure skips")
	}
}

// Low pressure never gates the lane.
func TestOrdersLanePassDispatchesBelowFSPressureThreshold(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	withFakePressureFile(t, []byte(samplePressureLow), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	if !od.called.Load() {
		t.Fatal("order dispatch did not run below the FS pressure threshold")
	}
}

// A tick the FS-pressure gate skips must not wake the orders lane: the tick
// never reached the point where it used to dispatch.
func TestFSPressureSkippedTickDoesNotWakeOrdersLane(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	cr.buildFnWithSessionBeads = func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
		t.Fatal("pressure-skipped tick reached its demand build")
		return DesiredStateResult{}
	}
	var dirty atomic.Bool
	var lastProviderName string
	var prevPoolRunning map[string]bool
	cr.tick(context.Background(), &dirty, &lastProviderName, cr.cityPath, &prevPoolRunning, "patrol")
	if n := len(cr.ordersLaneOf().wakeCh); n != 0 {
		t.Fatalf("pending lane wakes after a pressure-skipped tick = %d, want 0", n)
	}
}

// Under sustained pressure a poke-driven tick forces itself through every
// maxConsecutiveFSPressureSkips+1 ticks, and each force ends the tick's
// episode. That must not end the lane's: with a tick force between every
// lane pass, the lane still forces its own pass within
// maxConsecutiveFSPressureSkips+1 passes, or order dispatch starves.
func TestOrdersLaneForcesPassUnderPressureDespiteTickForces(t *testing.T) {
	od := &recordingOrderDispatcher{}
	var stderr bytes.Buffer
	cr := ordersLaneTestRuntime(t, od, "1h", &stderr)
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	cr.managedDoltOwned = func(string) (bool, error) { return false, nil }

	for pass := 1; pass <= maxConsecutiveFSPressureSkips+1; pass++ {
		tickForced := false
		for i := 0; i <= maxConsecutiveFSPressureSkips; i++ {
			if !cr.shouldSkipTickForFSPressure(nil, "poke") {
				tickForced = true
			}
		}
		if !tickForced {
			t.Fatalf("before lane pass %d: the tick never forced itself through", pass)
		}
		cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonWake)
		if od.called.Load() {
			if pass != maxConsecutiveFSPressureSkips+1 {
				t.Fatalf("lane forced dispatch on pass %d, want pass %d", pass, maxConsecutiveFSPressureSkips+1)
			}
			return
		}
	}
	t.Fatalf("no lane dispatch in %d passes under sustained pressure with tick forces between them: orders starve", maxConsecutiveFSPressureSkips+1)
}

// A pass the FS gate skips does not refresh the last-pass age the tick trace
// reports, so a lane starved by pressure shows a growing age rather than a
// fresh one.
func TestOrdersLanePressureSkippedPassIsNotALastPass(t *testing.T) {
	od := &recordingOrderDispatcher{}
	cr := ordersLaneTestRuntime(t, od, "1h", nil)
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	lane := cr.ordersLaneOf()

	cr.runOrdersLanePass(context.Background(), cr.cityPath, ordersLaneReasonCadence)
	if _, _, ran := lane.lastPass(); ran {
		t.Fatal("a pressure-skipped pass was recorded as the lane's last pass")
	}
}
