//go:build linux

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	retentionPressureSkipLine   = "skipping the order-tracking retention sweep"
	retentionPressureForcedLine = "forcing the order-tracking retention sweep"
)

// pressuredRetentionRuntime is a retention lane runtime over prunable beads
// with a fresh backup, its FS pressure read from the fake pressure file at
// the default threshold.
func pressuredRetentionRuntime(t *testing.T) (*CityRuntime, beads.Store, *syncWriter) {
	t.Helper()
	now := time.Now()
	city := beads.NewMemStoreFrom(100, retentionEligibleSeed("press", minClosedOrderTrackingRetained+2, now), nil)
	cr, stderr := retentionLaneRuntime(t, now, time.Hour, city, nil)
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")
	return cr, city, stderr
}

// Under high FS pressure the retention lane sheds its sweep like the orders
// lane sheds dispatch: maxConsecutiveFSPressureSkips passes skip, with one
// log line for the episode and the interval stamp untouched, and the next
// pass is forced through so retention cannot starve.
func TestOrderTrackingRetentionLaneSkipsSweepUnderFSPressure(t *testing.T) {
	cr, city, stderr := pressuredRetentionRuntime(t)

	for i := 0; i < maxConsecutiveFSPressureSkips; i++ {
		cr.runOrderTrackingRetentionLanePass(context.Background())
	}
	assertBeadsKept(t, city, "press-00", "press-01")
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want zero after pressure-skipped passes", cr.orderTrackingRetentionWatchdogLast)
	}
	out := stderr.String()
	if n := strings.Count(out, retentionPressureSkipLine); n != 1 {
		t.Fatalf("skip lines = %d, want 1 per pressure episode; stderr = %q", n, out)
	}

	cr.runOrderTrackingRetentionLanePass(context.Background())
	assertBeadsGone(t, city, "press-00", "press-01")
	out = stderr.String()
	if !strings.Contains(out, retentionPressureForcedLine) {
		t.Fatalf("stderr = %q, want the forced-sweep line", out)
	}
	if !strings.Contains(out, "pruned 2 closed bead(s)") {
		t.Fatalf("stderr = %q, want the forced pass to prune", out)
	}
}

// A low-pressure pass ends the episode: the next high-pressure pass logs a new
// skip line and the skip count starts over.
func TestOrderTrackingRetentionLaneLowPressureResetsEpisode(t *testing.T) {
	cr, _, stderr := pressuredRetentionRuntime(t)
	// The sweep itself is not under test here; a consumed stamp keeps the
	// low-pressure pass from deleting.
	cr.orderTrackingRetentionWatchdogLast = time.Now()

	for i := 0; i < maxConsecutiveFSPressureSkips-1; i++ {
		cr.runOrderTrackingRetentionLanePass(context.Background())
	}
	withFakePressureFile(t, []byte(samplePressureLow), nil)
	cr.runOrderTrackingRetentionLanePass(context.Background())
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	for i := 0; i < maxConsecutiveFSPressureSkips; i++ {
		cr.runOrderTrackingRetentionLanePass(context.Background())
	}

	out := stderr.String()
	if n := strings.Count(out, retentionPressureSkipLine); n != 2 {
		t.Fatalf("skip lines = %d, want 2 (one per episode); stderr = %q", n, out)
	}
	if strings.Contains(out, retentionPressureForcedLine) {
		t.Fatalf("stderr = %q, want no forced sweep: the low-pressure pass reset the skip count", out)
	}
}
