package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestRunPreparedStartCandidate_SessionExistsUncommittedOursConverges is
// AC-4 of gastownhall/gascity#6417: when the runtime under an uncommitted,
// prompt-bearing row carries this attempt's instance token — the runtime this
// very attempt launched, as a raced async retry leaves behind — the start
// converges on it and commits exactly as it does today: no launch, no stop,
// the committed hash landing with it. The verdict is asserted as the
// invariant (converge plus commit), because the pre-#6417 tree reaches the
// same commit through warm reuse instead of the collision classification.
func TestRunPreparedStartCandidate_SessionExistsUncommittedOursConverges(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
	row := createUncommittedStartRow(t, store, "gc-uncommitted", map[string]string{
		"state": "active", "session_key": "key-1", "continuation_epoch": "1",
		"provider": "claude", "transport": "tmux", "command": "claude", "work_dir": t.TempDir(),
	})
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), row.Metadata["session_name"], runtime.Config{}); err != nil {
		t.Fatalf("planting the attempt's own runtime: %v", err)
	}
	for key, value := range map[string]string{
		"GC_SESSION_ID":     row.ID,
		"GC_INSTANCE_TOKEN": "tok-ours", // this attempt's mint, on the row's token
		"GC_RUNTIME_EPOCH":  "2",
	} {
		if err := sp.SetMeta(row.Metadata["session_name"], key, value); err != nil {
			t.Fatalf("stamping the attempt's runtime %s: %v", key, err)
		}
	}
	city := t.TempDir()
	item, release := leasedStart(t, city, ownLaunchTestItem(t, row, "rendered startup prompt"))
	defer release()
	item.ownLaunchRequired = true
	item.coreHash = "hash-new"

	name := row.Metadata["session_name"]
	starts, stops := sp.CountCalls("Start", name), sp.CountCalls("Stop", name)
	result := runPreparedStartCandidate(context.Background(), item, city, sp, store, nil, 10*time.Second, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)

	if result.err != nil {
		t.Fatalf("a runtime carrying this attempt's token converges, got %v", result.err)
	}
	if result.outcome != TraceOutcomeSuccess && result.outcome != TraceOutcomeSessionExists {
		t.Fatalf("outcome = %q, want the converge family (success or session_exists), never a deferral", result.outcome)
	}
	if got := sp.CountCalls("Start", name); got != starts {
		t.Fatalf("Start calls = %d, want unchanged at %d: the runtime is this start's own", got, starts)
	}
	if got := sp.CountCalls("Stop", name); got != stops {
		t.Fatalf("Stop calls = %d, want unchanged at %d", got, stops)
	}
	if commitStartResultTraced(result, sessionFrontDoor(store), clk, events.NewFake(), 0, io.Discard, io.Discard, nil) != startCommitSucceeded {
		t.Fatal("the converged start must commit as it does today")
	}
	gotRow, err := store.Get(row.ID)
	if err != nil {
		t.Fatalf("get row after commit: %v", err)
	}
	if got := gotRow.Metadata["started_config_hash"]; got != "hash-new" {
		t.Fatalf("started_config_hash = %q, want the start's hash committed", got)
	}
}
