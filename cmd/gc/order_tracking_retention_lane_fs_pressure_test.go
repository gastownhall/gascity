//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The retention lane sheds under FS pressure like the orders lane does: the
// first maxConsecutiveFSPressureSkips passes skip (one log line per episode,
// interval stamp untouched) and the next pass is forced through (AC-6). A pass
// below the threshold resets the episode, so a later episode logs its own skip
// line instead of forcing straight through.
func TestOrderTrackingRetentionLaneSkipsUnderFSPressure(t *testing.T) {
	// Real time: the lane stamps its passes with time.Now(), so the backup age
	// and the bead ages must be measured against it, not a fake date.
	now := time.Now()
	cr, store, stderrBuf := seedRetentionWatchdogCity(t, now, time.Hour)
	episode := &retentionLaneFSEpisode{}
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	t.Setenv(fsPressureThresholdEnv, "")

	for i := 0; i < maxConsecutiveFSPressureSkips; i++ {
		cr.runOrderTrackingRetentionLanePass(context.Background(), episode)
	}
	for i := range minClosedOrderTrackingRetained + 2 {
		id := fmt.Sprintf("guard-%02d", i)
		if _, err := store.Get(id); err != nil {
			t.Fatalf("%s should be preserved while pressure sheds the pass: %v", id, err)
		}
	}
	if !cr.orderTrackingRetentionWatchdogLast.IsZero() {
		t.Fatalf("orderTrackingRetentionWatchdogLast = %v, want zero: a pressure-skipped pass leaves the stamp untouched", cr.orderTrackingRetentionWatchdogLast)
	}
	out := stderrBuf.String()
	if !strings.Contains(out, "FS pressure high") || !strings.Contains(out, "skipping order-tracking retention sweep") {
		t.Fatalf("stderr = %q, want a retention FS pressure skip warning", out)
	}
	if n := strings.Count(out, "skipping order-tracking retention sweep"); n != 1 {
		t.Fatalf("skip warnings = %d, want 1 per pressure episode", n)
	}

	// The next pass is forced through, as the orders lane's gate does.
	cr.runOrderTrackingRetentionLanePass(context.Background(), episode)
	if _, err := store.Get("guard-00"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(guard-00) err = %v, want ErrNotFound: the forced pass sweeps", err)
	}
	if got := stderrBuf.String(); !strings.Contains(got, "forcing order-tracking retention sweep") {
		t.Fatalf("stderr = %q, want the forced-pass notice", got)
	}

	// A pass below the threshold resets the episode: the next high-pressure
	// pass skips (and logs) again instead of forcing again.
	withFakePressureFile(t, []byte(samplePressureLow), nil)
	cr.runOrderTrackingRetentionLanePass(context.Background(), episode)
	withFakePressureFile(t, []byte(samplePressureHigh), nil)
	cr.runOrderTrackingRetentionLanePass(context.Background(), episode)
	if n := strings.Count(stderrBuf.String(), "skipping order-tracking retention sweep"); n != 2 {
		t.Fatalf("skip warnings = %d, want 2 after the low-pressure pass reset the episode", n)
	}
}
