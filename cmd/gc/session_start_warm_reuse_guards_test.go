package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// warmReuseGuardHarness plants a live, healthy runtime under a row's name and
// runs a start whose preparedStart does not require its own launch (a row
// whose started_config_hash was set before PreWake — a committed row, or a
// wake_mode = "fresh" warm pool slot). Such a start must warm-reuse the
// runtime unchanged (#6417 AC-5): no launch, no stop, a nil error.
func warmReuseGuardHarness(t *testing.T, extraMeta map[string]string) {
	t.Helper()
	store := beads.NewMemStore()
	row := createUncommittedStartRow(t, store, "gc-committed", map[string]string{"state": "active"})
	for k, v := range extraMeta {
		if k == "state" {
			continue
		}
		row.Metadata[k] = v
		if err := store.SetMetadata(row.ID, k, v); err != nil {
			t.Fatalf("SetMetadata(%s): %v", k, err)
		}
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), row.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("planting the warm runtime: %v", err)
	}
	if err := sp.SetMeta(row.Metadata["session_name"], "GC_SESSION_ID", row.ID); err != nil {
		t.Fatalf("stamping the warm runtime: %v", err)
	}

	item := ownLaunchTestItem(t, row, "rendered startup prompt")
	item.ownLaunchRequired = false // committed rows never require their own launch (prepare decides this pre-PreWake)
	name := row.Metadata["session_name"]
	starts, stops := sp.CountCalls("Start", name), sp.CountCalls("Stop", name)

	started, err := startPreparedStartCandidate(context.Background(), item, t.TempDir(), store, sp, nil, nil, immediateSessionStaleKeyDetectionWaiter, nil)
	if err != nil {
		t.Fatalf("warm reuse over a committed row: %v", err)
	}
	if started {
		t.Fatal("warm reuse launches nothing")
	}
	if got := sp.CountCalls("Start", name); got != starts {
		t.Fatalf("Start calls = %d, want unchanged at %d", got, starts)
	}
	if got := sp.CountCalls("Stop", name); got != stops {
		t.Fatalf("Stop calls = %d, want unchanged at %d", got, stops)
	}
}

// TestStartPreparedStartCandidate_CommittedRowWarmReuseUnchanged is AC-5 of
// gastownhall/gascity#6417: a row whose started_config_hash was set before
// PreWake keeps warm reuse, its warm-bind claim nudge and its commit exactly
// as before — the own-launch rule never applies to a committed row.
func TestStartPreparedStartCandidate_CommittedRowWarmReuseUnchanged(t *testing.T) {
	warmReuseGuardHarness(t, map[string]string{"started_config_hash": "hash-before"})
}

// TestStartPreparedStartCandidate_FreshModePoolSlotWarmReuseUnchanged is
// AC-5 of gastownhall/gascity#6417: a wake_mode = "fresh" warm pool slot
// carries its started_config_hash into the wake, so PreWake's hash clear finds
// nothing to record and the slot keeps warm reuse of its live runtime.
func TestStartPreparedStartCandidate_FreshModePoolSlotWarmReuseUnchanged(t *testing.T) {
	warmReuseGuardHarness(t, map[string]string{"wake_mode": "fresh", "started_config_hash": "hash-before"})
}
