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

// TestRunPreparedStartCandidate_SessionExistsNotLiveDefers is AC-3 of
// gastownhall/gascity#6417: a provider start that collides with the name whose
// post-collision observation reads not live (or fails) is deferred on any row,
// never a terminal provider_error, and records no wake failure — a terminal
// classification left the row neither started nor retrying.
func TestRunPreparedStartCandidate_SessionExistsNotLiveDefers(t *testing.T) {
	cases := []struct {
		name    string
		addMeta map[string]string
		own     bool
	}{
		{name: "committed row", addMeta: map[string]string{"started_config_hash": "hash-before"}},
		{name: "uncommitted prompt row", addMeta: map[string]string{}, own: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}
			meta := map[string]string{
				"wake_attempts":      "0",
				"continuation_epoch": "1",
				"provider":           "claude",
				"transport":          "tmux",
				"command":            "claude",
				"work_dir":           t.TempDir(),
				"resume_flag":        "--resume",
				"resume_style":       "flag",
				"session_key":        "key-1",
			}
			for k, v := range tc.addMeta {
				meta[k] = v
			}
			row := createUncommittedStartRow(t, store, "gc-row", meta)
			sp := runtime.NewFake()
			sp.StartErrors[row.Metadata["session_name"]] = runtime.ErrSessionExists
			city := t.TempDir()
			item, release := leasedStart(t, city, ownLaunchTestItem(t, row, "rendered startup prompt"))
			defer release()
			item.ownLaunchRequired = tc.own

			result := runPreparedStartCandidate(context.Background(), item, city, sp, store, nil, 10*time.Second, immediateStartStabilityWaiter, immediateSessionStaleKeyDetectionWaiter, nil)

			if result.outcome != TraceOutcomeDeferred {
				t.Fatalf("outcome = %q, want %q: a colliding runtime whose observation reads not live is a deferral, not a terminal classification", result.outcome, TraceOutcomeDeferred)
			}
			if result.err != nil {
				t.Fatalf("a deferral carries no terminal error, got %v", result.err)
			}
			if commitStartResultTraced(result, sessionFrontDoor(store), clk, events.Discard, 0, io.Discard, io.Discard, nil) == startCommitSucceeded {
				t.Fatal("a deferred start must not commit")
			}
			gotRow, err := store.Get(row.ID)
			if err != nil {
				t.Fatalf("get row after commit: %v", err)
			}
			if got := gotRow.Metadata["wake_attempts"]; got != "0" {
				t.Fatalf("wake_attempts = %q, want \"0\": a collision that reads not live records no wake failure", got)
			}
		})
	}
}
