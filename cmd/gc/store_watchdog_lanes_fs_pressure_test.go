//go:build linux

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// storeWatchdogLastRun is the clock the watchdog key's body sets when it runs.
func storeWatchdogLastRun(cr *CityRuntime, key storeWatchdogKey) time.Time {
	switch key {
	case storeWatchdogOrderTrackingSweep:
		return cr.orderSweepWatchdogLast
	case storeWatchdogOrderTrackingRetention:
		return cr.orderTrackingRetentionWatchdogLast
	case storeWatchdogNudgeMailSweep:
		return cr.nudgeMailSweepWatchdogLast
	}
	return time.Time{}
}

// Under sustained FS pressure each store watchdog lane sheds its due passes
// on an episode of its own: no shed pass reaches a store, and each lane
// forces its own pass on attempt maxConsecutiveFSPressureSkips+1 however
// often the tick and the orders lane force theirs in between. Each lane logs
// its skip once per episode, naming its own work.
func TestStoreWatchdogLanesShedFSPressureOnTheirOwnEpisodes(t *testing.T) {
	now := time.Now()
	store := &watchdogOpCountingStore{Store: beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)}
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
	var stderr bytes.Buffer
	cr.stderr = &stderr
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	ctx := context.Background()
	lanes := cr.storeWatchdogLanesOf()

	for attempt := 1; attempt <= maxConsecutiveFSPressureSkips+1; attempt++ {
		for i := 0; i <= maxConsecutiveFSPressureSkips; i++ {
			cr.shouldSkipTickForFSPressure(nil, "poke")
		}
		cr.runOrdersLanePass(ctx, cr.cityPath, ordersLaneReasonCadence)
		forced := attempt == maxConsecutiveFSPressureSkips+1
		for _, lane := range lanes {
			before := store.ops.Load()
			cr.runStoreWatchdogLanePass(ctx, lane)
			ran := !storeWatchdogLastRun(cr, lane.spec.key).IsZero()
			if ran != forced {
				t.Fatalf("attempt %d: %s ran = %v, want %v (forced on attempt %d)",
					attempt, lane.spec.work, ran, forced, maxConsecutiveFSPressureSkips+1)
			}
			if !forced && store.ops.Load() != before {
				t.Fatalf("attempt %d: shed %s pass made %d store calls, want 0",
					attempt, lane.spec.work, store.ops.Load()-before)
			}
		}
	}

	out := stderr.String()
	for _, lane := range lanes {
		if n := strings.Count(out, "skipping "+lane.spec.work+"\n"); n != 1 {
			t.Fatalf("%q skip warnings = %d, want 1 per pressure episode; stderr:\n%s", lane.spec.work, n, out)
		}
		forcedLine := "forcing " + lane.spec.work + " after 5 skipped passes"
		if !strings.Contains(out, forcedLine) {
			t.Fatalf("stderr lacks %q:\n%s", forcedLine, out)
		}
	}
	if n := strings.Count(out, "skipping order dispatch\n"); n != 1 {
		t.Fatalf("order dispatch skip warnings = %d, want 1; stderr:\n%s", n, out)
	}
}

// Low pressure never gates a lane: every due watchdog runs on its first pass.
func TestStoreWatchdogLanesRunBelowFSPressureThreshold(t *testing.T) {
	now := time.Now()
	store := beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
	withFakePressureFile(t, []byte(samplePressureLow), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	for _, lane := range cr.storeWatchdogLanesOf() {
		cr.runStoreWatchdogLanePass(context.Background(), lane)
		if storeWatchdogLastRun(cr, lane.spec.key).IsZero() {
			t.Fatalf("%s did not run below the FS pressure threshold", lane.spec.work)
		}
	}
}

// A pass the FS gate sheds is not a completed pass: the lane's age keeps
// growing while its last status says why.
func TestStoreWatchdogLanePressureSkippedPassIsNotACompletedPass(t *testing.T) {
	now := time.Now()
	store := beads.NewMemStoreFrom(100, storeWatchdogSeed(now), nil)
	cr := storeWatchdogTestRuntime(t, store, &recordingOrderDispatcher{})
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	lane := cr.storeWatchdogLaneOf(storeWatchdogOrderTrackingRetention)
	cr.runStoreWatchdogLanePass(context.Background(), lane)
	if st := lane.status(); st.completed || st.last != storeWatchdogPassPressureSkipped {
		t.Fatalf("status after a shed pass = %q completed=%v, want %q and no completed pass", st.last, st.completed, storeWatchdogPassPressureSkipped)
	}
}
