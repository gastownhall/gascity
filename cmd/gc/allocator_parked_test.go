package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// A parked seat's binding keeps the work dir its conversation ran in, and
// carries it (KeptWorkDir) for the PreWake to verify: while the seat parks,
// once its pin is ready, and when the binding writes nothing else (its
// trigger already names the pin, as after a wake whose launch failed once its
// PreWake landed), so a kept dir deleted since is still caught before the
// launch.
func TestAllocator_ParkedSeatsBindingKeepsItsRecordedWorkDir(t *testing.T) {
	const recorded = "/city/.gc/worktrees/step-1"
	for _, tc := range []struct {
		name, trigger string
		ready         bool
	}{
		{name: "parked", trigger: "step-1"},
		{name: "its pin ready", trigger: "step-1", ready: true},
		{name: "its pin ready, its trigger already the pin", trigger: "pin-1", ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Agents: []config.Agent{{Name: "worker", WorkDir: ".gc/worktrees/{{.AgentBase}}"}}}
			row := poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", ago(time.Hour),
				beadmeta.WorkDirMetadataKey, recorded, beadmeta.LegacyWorkDirMetadataKey, recorded, beadmeta.TriggerBeadIDMetadataKey, tc.trigger)
			f := newAllocFixture(t, cfg).sessions(row)
			f.in.Demand.AssignedWork = []beads.Bead{{ID: "pin-1", Type: "task", Status: "open", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": "worker"}}}
			f.in.Demand.AssignedStoreRefs = []string{""}
			f.in.Demand.ReadyAssigned = map[storeScopedBeadKey]bool{{StoreRef: "", ID: "pin-1"}: tc.ready}
			e := entryOf(t, f.decide(), "gc-1")
			if e.Binding == nil || e.Binding.KeptWorkDir != recorded || e.Binding.WorkDir == recorded {
				t.Fatalf("binding %+v, want the recorded %s kept over a derived dir", e.Binding, recorded)
			}
			for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.LegacyWorkDirMetadataKey} {
				if got, ok := e.Binding.Patch[key]; ok {
					t.Fatalf("binding patch %s = %q, want the recorded dir kept", key, got)
				}
			}
			if got, want := e.Binding.Patch[beadmeta.TriggerBeadIDMetadataKey], map[bool]string{true: "pin-1", false: ""}[tc.trigger != "pin-1"]; got != want {
				t.Fatalf("binding trigger patch %q, want %q", got, want)
			}
		})
	}
}

// A ready pin at a full city cap is a counted resume that competes with the
// live seat holding the cap's one slot, by bead priority (P1 outranks the live
// seat's P2 work, P3 does not; ties are the planner test's). The cap counts
// requests, not seats: a request it rejects selects no seat, but the overlay
// keeps every open row of a template the pass still selects. So a seat that
// loses to a seat of its own template stays selected, and wakes for its work
// past the cap. One whose template keeps no selected row drops out: a parked
// seat's ready pin that loses leaves an orphan Drain holding the pin, which
// A21's orphan close releases (SESS-082); a live seat the pin displaces is a
// Drain holding its claimed work, which keeps it running until that work is
// done (SESS-074).
func TestAllocator_ReadyPinCompetesForAFullCityCap(t *testing.T) {
	decide := func(t *testing.T, liveTemplate string, pinPriority int) allocDecision {
		wsMax := 1
		cfg := &config.City{Workspace: config.Workspace{MaxActiveSessions: &wsMax}, Agents: []config.Agent{{Name: "worker"}}}
		if liveTemplate != "worker" {
			cfg.Agents = append(cfg.Agents, config.Agent{Name: liveTemplate})
		}
		f := newAllocFixture(t, cfg).sessions(
			poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", ago(time.Hour)),
			poolRow("gc-2", liveTemplate, 2, "active"),
		).alive("s-gc-2", InventoryAttrs{})
		claimed, pinned := 2, pinPriority
		f.in.Demand.AssignedWork = []beads.Bead{
			{ID: "w-2", Type: "task", Status: "in_progress", Assignee: "gc-2", Priority: &claimed, Metadata: map[string]string{"gc.routed_to": liveTemplate}},
			{ID: "pin-1", Type: "task", Status: "open", Assignee: "gc-1", Priority: &pinned, Metadata: map[string]string{"gc.routed_to": "worker"}},
		}
		f.in.Demand.AssignedStoreRefs = []string{"", ""}
		f.in.Demand.ReadyAssigned = map[storeScopedBeadKey]bool{{StoreRef: "", ID: "pin-1"}: true}
		return f.decide()
	}
	wakes := func(t *testing.T, d allocDecision, id, work string) {
		t.Helper()
		if e := entryOf(t, d, id); !e.InDesired || e.Desired != desireWake || e.Reason != reasonAssignedWork || e.AssignedWork == nil || e.AssignedWork.BeadID != work {
			t.Fatalf("%s = %s/%s in desired %v work %+v, want selected and woken for %s", id, e.Desired, e.Reason, e.InDesired, e.AssignedWork, work)
		}
	}
	drains := func(t *testing.T, d allocDecision, id, work string) {
		t.Helper()
		if e := entryOf(t, d, id); e.InDesired || e.Desired != desireDrain || e.DrainReason != drainOrphaned || e.OpenWork == nil || e.OpenWork.BeadID != work {
			t.Fatalf("%s = %s/%s in desired %v drain %q open work %+v, want an orphan drain holding %s", id, e.Desired, e.Reason, e.InDesired, e.DrainReason, e.OpenWork, work)
		}
	}
	t.Run("a pin that loses to its own template still wakes past the cap", func(t *testing.T) {
		d := decide(t, "worker", 3)
		if e := entryOf(t, d, "gc-1"); e.Park.Parked || !e.Park.Eligible {
			t.Fatalf("gc-1 verdict %+v, want eligible and not parked: its pin is ready", e.Park)
		}
		wakes(t, d, "gc-1", "pin-1")
		wakes(t, d, "gc-2", "w-2")
	})
	t.Run("a live seat of the pin's template it outranks keeps running past the cap", func(t *testing.T) {
		d := decide(t, "worker", 1)
		wakes(t, d, "gc-1", "pin-1")
		wakes(t, d, "gc-2", "w-2")
	})
	t.Run("a pin that loses with no selected row of its template is an orphan drain", func(t *testing.T) {
		d := decide(t, "other", 3)
		drains(t, d, "gc-1", "pin-1")
		wakes(t, d, "gc-2", "w-2")
	})
	t.Run("a live seat of another template the pin outranks drains holding its work", func(t *testing.T) {
		d := decide(t, "other", 1)
		wakes(t, d, "gc-1", "pin-1")
		drains(t, d, "gc-2", "w-2")
	})
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
