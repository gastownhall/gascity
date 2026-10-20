package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// The v2 allocator's half of a parked seat (ga-ylpbsy.16): a seat asleep on
// its drain-ack holding only a blocked pin stays selected (Sleep, never an
// orphan Drain), carries its verdict for A21, and is no pool demand; the
// same seat wakes in place once the pin is ready.

func parkedAllocFixture(t *testing.T, sleptAgo time.Duration) *allocFixture {
	t.Helper()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	row := poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", ago(sleptAgo))
	f := newAllocFixture(t, cfg).sessions(row)
	f.in.Demand.AssignedWork = []beads.Bead{{ID: "pin-1", Type: "task", Status: "open", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}}
	f.in.Demand.AssignedStoreRefs = []string{""}
	f.in.Demand.ReadyAssigned = map[storeScopedBeadKey]bool{}
	return f
}

func TestAllocator_ParkedSeatSleepsSelectedWithItsVerdict(t *testing.T) {
	f := parkedAllocFixture(t, time.Hour)
	d := f.decide()
	e := entryOf(t, d, "gc-1")
	if e.Desired != desireSleep || !e.InDesired {
		t.Fatalf("gc-1 = %s/%s in desired %v, want a selected Sleep: never an orphan drain", e.Desired, e.Reason, e.InDesired)
	}
	if !e.Park.Parked || !e.Park.Eligible || !e.Park.Until.Equal(allocNow.Add(-time.Hour).Add(parkedWorkMaxHold)) {
		t.Fatalf("park verdict %+v, want parked until slept_at plus the bound", e.Park)
	}
	if got := d.Snapshot.PoolDesired["worker"]; got != 0 {
		t.Fatalf("pool desired = %d, want 0: a parked seat is no pool demand", got)
	}
}

func TestAllocator_ReadyPinWakesTheParkedSeatInPlace(t *testing.T) {
	f := parkedAllocFixture(t, time.Hour)
	f.in.Demand.ReadyAssigned = map[storeScopedBeadKey]bool{{StoreRef: "", ID: "pin-1"}: true}
	d := f.decide()
	e := entryOf(t, d, "gc-1")
	if e.Desired != desireWake || e.Reason != reasonAssignedWork || e.AssignedWork == nil || e.AssignedWork.BeadID != "pin-1" {
		t.Fatalf("gc-1 = %s/%s work %+v, want the assigned-work wake on pin-1", e.Desired, e.Reason, e.AssignedWork)
	}
	if e.Park.Parked {
		t.Fatalf("park verdict %+v, want not parked once the pin is ready", e.Park)
	}
}

func TestAllocator_ParkedSeatPastItsBoundIsNotParked(t *testing.T) {
	f := parkedAllocFixture(t, parkedWorkMaxHold+time.Minute)
	e := entryOf(t, f.decide(), "gc-1")
	if e.Park.Parked || e.Park.Eligible {
		t.Fatalf("park verdict %+v, want none past the bound", e.Park)
	}
	if e.Desired != desireSleep {
		t.Fatalf("gc-1 = %s/%s, want Sleep: A21 then takes the stranded path", e.Desired, e.Reason)
	}
}

func TestAllocator_PartialWorkReadParksNothing(t *testing.T) {
	f := parkedAllocFixture(t, time.Hour)
	f.in.Demand.StorePartial = true
	if e := entryOf(t, f.decide(), "gc-1"); e.Park != (parkVerdict{}) {
		t.Fatalf("park verdict %+v, want none on a partial work read", e.Park)
	}
}

// A full slot-bounded pool unparks its oldest seat (owner decision D2b).
func TestAllocator_FullBoundedPoolUnparksItsOldestSeat(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{allocPoolAgent("worker", 2)}}
	f := newAllocFixture(t, cfg).sessions(
		poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", ago(2*time.Hour)),
		poolRow("gc-2", "worker", 2, "asleep", "sleep_reason", "idle", "slept_at", ago(time.Hour)),
	)
	f.in.Demand.AssignedWork = []beads.Bead{
		{ID: "pin-1", Type: "task", Status: "open", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}},
		{ID: "pin-2", Type: "task", Status: "open", Assignee: "gc-2", Metadata: map[string]string{"gc.routed_to": "worker"}},
	}
	f.in.Demand.AssignedStoreRefs = []string{"", ""}
	f.in.Demand.ReadyAssigned = map[storeScopedBeadKey]bool{}
	d := f.decide()
	if e := entryOf(t, d, "gc-1"); e.Park.Parked || e.Park.Eligible {
		t.Fatalf("gc-1 verdict %+v, want the oldest seat unparked", e.Park)
	}
	if e := entryOf(t, d, "gc-2"); !e.Park.Parked {
		t.Fatalf("gc-2 verdict %+v, want parked", e.Park)
	}
}
