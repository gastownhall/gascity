//go:build linux

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
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
