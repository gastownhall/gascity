package main

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestHookClaimLiveClaimantStaysAClaimRejection: a claim lost to another live
// session (the canonical row is in progress under it) is a race, reported as
// bead.claim_rejected and never as a claim failure, and an otherwise empty hook
// drains no_work. A refusal whose readback came back empty reports neither.
func TestHookClaimLiveClaimantStaysAClaimRejection(t *testing.T) {
	run := runHookClaimRefusals(t, []string{"work-v", "work-e"}, map[string]beads.Bead{
		"work-v": {ID: "work-v", Status: "in_progress", Assignee: "other-session"},
		"work-e": {},
	})
	if want := []string{"work-v", "work-e"}; !slices.Equal(run.claimed, want) {
		t.Fatalf("claims tried = %v, want %v", run.claimed, want)
	}
	if want := []string{"work-v"}; !slices.Equal(run.rejected, want) {
		t.Fatalf("claim_rejected for %v, want %v", run.rejected, want)
	}
	if len(run.failed) != 0 {
		t.Fatalf("claim failures = %v, want none: a lost race is not a failed claim", run.failed)
	}
	if run.result.Action != "drain" || run.result.Reason != hookClaimReasonNoWork {
		t.Fatalf("claim result = %+v, want drain/%s", run.result, hookClaimReasonNoWork)
	}
}
