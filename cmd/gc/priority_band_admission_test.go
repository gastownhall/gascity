package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestSortSessionRequestsPreservesRecoveryThenPriorityBandFIFO(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	p0, p2, p4 := 0, 2, 4
	requests := []SessionRequest{
		{Template: "worker", Tier: "new", BeadPriority: &p2, WorkBeadID: "p2-old", WorkCreatedAt: base},
		{Template: "worker", Tier: "new", BeadPriority: &p0, WorkBeadID: "p0-new", WorkCreatedAt: base.Add(time.Hour)},
		{Template: "worker", Tier: "resume", BeadPriority: &p4, SessionBeadID: "session", WorkBeadID: "recovery", WorkCreatedAt: base.Add(2 * time.Hour)},
	}

	sortSessionRequests(requests)
	got := []string{requests[0].WorkBeadID, requests[1].WorkBeadID, requests[2].WorkBeadID}
	want := []string{"recovery", "p0-new", "p2-old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request order = %v, want %v", got, want)
	}
}

func TestSortScaleCheckDemandUsesPriorityBandFIFO(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	demand := scaleCheckDemand{
		WorkBeadIDs: []string{"p1-old", "p0-new", "p0-old"},
		Priorities:  map[string]int{"p1-old": 1, "p0-new": 0, "p0-old": 0},
		CreatedAt: map[string]time.Time{
			"p1-old": base.Add(-time.Hour),
			"p0-new": base.Add(time.Hour),
			"p0-old": base,
		},
	}

	sortScaleCheckDemand(&demand)
	want := []string{"p0-old", "p0-new", "p1-old"}
	if !reflect.DeepEqual(demand.WorkBeadIDs, want) {
		t.Fatalf("WorkBeadIDs = %v, want %v", demand.WorkBeadIDs, want)
	}
}

func TestFreshCreatePrioritiesPutTheFloorReservationFirst(t *testing.T) {
	p3, p2 := 3, 2
	requests := []SessionRequest{
		{Tier: "new", BeadPriority: &p3},
		{Tier: "new", BeadPriority: &p2, FloorGuarantee: true},
	}

	got := freshCreatePriorities(requests)
	if want := []int{2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh priorities = %v, want floor-first %v", got, want)
	}
}

func TestMergeScaleCheckDemandRestoresPriorityBandFIFO(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	existing := scaleCheckDemand{
		WorkBeadIDs: []string{"p4-old"},
		Priorities:  map[string]int{"p4-old": 4},
		CreatedAt:   map[string]time.Time{"p4-old": base},
	}
	incoming := scaleCheckDemand{
		WorkBeadIDs: []string{"p0-new"},
		Priorities:  map[string]int{"p0-new": 0},
		CreatedAt:   map[string]time.Time{"p0-new": base.Add(time.Hour)},
	}

	got := mergeScaleCheckDemand(existing, incoming, 1)
	want := []string{"p0-new", "p4-old"}
	if !reflect.DeepEqual(got.WorkBeadIDs, want) {
		t.Fatalf("merged WorkBeadIDs = %v, want %v", got.WorkBeadIDs, want)
	}
}

func TestOrderedHookCandidatesUsesPriorityBandFIFO(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	p0, p1 := 0, 1
	got := []beads.Bead{
		{ID: "p1-old", Priority: &p1, CreatedAt: base},
		{ID: "p0-new", Priority: &p0, CreatedAt: base.Add(time.Hour)},
		{ID: "p0-old", Priority: &p0, CreatedAt: base},
	}
	orderHookCandidates(got)
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	want := []string{"p0-old", "p0-new", "p1-old"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("candidate order = %v, want %v", ids, want)
	}
}

func TestOrderedHookCandidatesTreatsMalformedRankAsLeastUrgent(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	p0, invalid := 0, -1
	got := []beads.Bead{
		{ID: "invalid-priority", Priority: &invalid, CreatedAt: base.Add(-time.Hour)},
		{ID: "missing-created", Priority: &p0},
		{ID: "valid", Priority: &p0, CreatedAt: base},
	}
	orderHookCandidates(got)
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	want := []string{"valid", "missing-created", "invalid-priority"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("candidate order = %v, want malformed ranks after valid work %v", ids, want)
	}
}

func TestHookRankBreaksSameTierTiesByFIFO(t *testing.T) {
	base := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	p0 := 0
	older := hookCandidateRank{tier: hookTierRouted, bead: beads.Bead{ID: "older", Priority: &p0, CreatedAt: base}}
	newer := hookCandidateRank{tier: hookTierRouted, bead: beads.Bead{ID: "newer", Priority: &p0, CreatedAt: base.Add(time.Hour)}}
	if !older.less(newer) {
		t.Fatal("older same-band candidate must rank before newer candidate across stores")
	}
}
