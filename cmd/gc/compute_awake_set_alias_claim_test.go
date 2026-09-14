package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// Pool seats claim work under their alias (GC_ALIAS = "rig/gastown.nux"),
// while their session bead's session_name is the tmux form
// ("rig--gastown__nux"). A seat holding an in_progress claim must stay
// desired ("assigned-work") when template demand drops, and its scale slot
// must be counted, otherwise the reconciler retires it mid-claim.
func TestComputeAwakeSet_PoolSeatClaimByAliasStaysDesired(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "tributary/gastown.polecat"}},
		SessionBeads: []AwakeSessionBead{
			{ID: "ac-nux", SessionName: "tributary--gastown__nux", Alias: "tributary/gastown.nux", Template: "tributary/gastown.polecat", State: "active", CreatedAt: now.Add(-time.Hour)},
			{ID: "ac-slit", SessionName: "tributary--gastown__slit", Alias: "tributary/gastown.slit", Template: "tributary/gastown.polecat", State: "active", CreatedAt: now.Add(-time.Hour)},
		},
		WorkBeads: []AwakeWorkBead{{ID: "tr-work", Assignee: "tributary/gastown.nux", Status: "in_progress"}},
		RunningSessions: map[string]bool{
			"tributary--gastown__nux":  true,
			"tributary--gastown__slit": true,
		},
		ScaleCheckCounts: map[string]int{},
		Now:              now,
	})
	d, ok := result["tributary--gastown__nux"]
	if !ok || !d.ShouldWake {
		t.Fatalf("alias claim holder must stay desired: %+v", d)
	}
	if d.Reason != "assigned-work" {
		t.Fatalf("alias claim holder reason = %q, want assigned-work", d.Reason)
	}
	if other := result["tributary--gastown__slit"]; other.ShouldWake {
		t.Fatalf("work-free seat must not be kept awake without demand: %+v", other)
	}
}

func TestSessionAssigneeMatches_Alias(t *testing.T) {
	bead := AwakeSessionBead{ID: "ac-1", SessionName: "rig--gastown__nux", Alias: "rig/gastown.nux", Template: "rig/gastown.polecat"}
	if !sessionAssigneeMatches(nil, bead, "rig/gastown.nux") {
		t.Fatal("alias must match")
	}
	if !sessionAssigneeMatches(nil, bead, "rig--gastown__nux") {
		t.Fatal("session_name must still match")
	}
	if sessionAssigneeMatches(nil, bead, "rig/gastown.slit") {
		t.Fatal("foreign alias must not match")
	}
}

func TestSessionAssignmentIdentifiersInfo_IncludesAlias(t *testing.T) {
	info := session.Info{ID: "ac-1", SessionNameMetadata: "rig--gastown__nux", Alias: "rig/gastown.nux"}
	got := sessionAssignmentIdentifiersInfo(info)
	want := map[string]bool{"ac-1": true, "rig--gastown__nux": true, "rig/gastown.nux": true}
	for _, id := range got {
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("identifiers %v missing %v", got, want)
	}
}
