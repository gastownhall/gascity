package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestNextDrainAckAssignedWorkCycle(t *testing.T) {
	now := time.Date(2026, 8, 5, 3, 0, 0, 0, time.UTC)

	t.Run("fresh bead starts at 1", func(t *testing.T) {
		got := nextDrainAckAssignedWorkCycle(beads.Bead{}, now)
		if got.cycles != 1 || got.tripped {
			t.Fatalf("got %+v, want cycles=1 tripped=false", got)
		}
	})

	t.Run("accumulates inside the window", func(t *testing.T) {
		wb := beads.Bead{Metadata: map[string]string{
			beadmeta.DrainAckAssignedWorkCycleCountMetadataKey:       "3",
			beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey: now.Add(-5 * time.Minute).Format(time.RFC3339),
		}}
		got := nextDrainAckAssignedWorkCycle(wb, now)
		if got.cycles != 4 || got.tripped {
			t.Fatalf("got %+v, want cycles=4 tripped=false", got)
		}
	})

	t.Run("trips at the cap", func(t *testing.T) {
		wb := beads.Bead{Metadata: map[string]string{
			beadmeta.DrainAckAssignedWorkCycleCountMetadataKey:       strconv.Itoa(drainAckAssignedWorkCycleCap - 1),
			beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey: now.Add(-1 * time.Minute).Format(time.RFC3339),
		}}
		got := nextDrainAckAssignedWorkCycle(wb, now)
		if got.cycles != drainAckAssignedWorkCycleCap || !got.tripped {
			t.Fatalf("got %+v, want cycles=%d tripped=true", got, drainAckAssignedWorkCycleCap)
		}
	})

	t.Run("stale window restarts the streak instead of accumulating", func(t *testing.T) {
		wb := beads.Bead{Metadata: map[string]string{
			beadmeta.DrainAckAssignedWorkCycleCountMetadataKey:       strconv.Itoa(drainAckAssignedWorkCycleCap - 1),
			beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey: now.Add(-2 * drainAckAssignedWorkCycleWindow).Format(time.RFC3339),
		}}
		got := nextDrainAckAssignedWorkCycle(wb, now)
		if got.cycles != 1 || got.tripped {
			t.Fatalf("got %+v, want a fresh streak (cycles=1, tripped=false) once the window has elapsed", got)
		}
		if !got.windowStart.Equal(now) {
			t.Fatalf("windowStart = %v, want now (%v) for a restarted streak", got.windowStart, now)
		}
	})

	t.Run("malformed metadata fails open to a fresh streak", func(t *testing.T) {
		wb := beads.Bead{Metadata: map[string]string{
			beadmeta.DrainAckAssignedWorkCycleCountMetadataKey:       "not-a-number",
			beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey: "not-a-timestamp",
		}}
		got := nextDrainAckAssignedWorkCycle(wb, now)
		if got.cycles != 1 || got.tripped {
			t.Fatalf("got %+v, want cycles=1 tripped=false on malformed metadata", got)
		}
	})
}

// redispatchCapTestFixture builds a session bead plus a work bead assigned to
// it, routed to "novices", the exact shape ra-3y4okc's stranded bead took
// (in_progress, assignee=<pool session>, routed to the pool that keeps
// escalating and draining it).
func redispatchCapTestFixture(t *testing.T, env *reconcilerTestEnv) (session beads.Bead, work beads.Bead) {
	t.Helper()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "novices"}}}
	session = env.createSessionBead("novices", "novices")
	work, err := env.store.Create(beads.Bead{
		Title:    "misrouted bead the pool may not close",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "novices",
		},
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	return session, work
}

func TestEnforceDrainAckAssignedWorkCycleCap_AccumulatesBelowCap(t *testing.T) {
	env := newReconcilerTestEnv()
	fake := events.NewFake()
	env.rec = fake
	session, work := redispatchCapTestFixture(t, env)

	for i := 1; i < drainAckAssignedWorkCycleCap; i++ {
		enforceDrainAckAssignedWorkCycleCap("", env.cfg, env.store, nil, env.sessionInfo(session.ID), env.clk, env.rec, &env.stderr)

		got, err := env.store.Get(work.ID)
		if err != nil {
			t.Fatalf("Get(work) after cycle %d: %v", i, err)
		}
		if got.Assignee != session.ID {
			t.Fatalf("after cycle %d: assignee = %q, want %q (must stay assigned below the cap)", i, got.Assignee, session.ID)
		}
		if beadHasLabel(got, beadmeta.HoldMayorLabel) {
			t.Fatalf("after cycle %d: bead already holds %s below the cap", i, beadmeta.HoldMayorLabel)
		}
		wantCount := strconv.Itoa(i)
		if got.Metadata[beadmeta.DrainAckAssignedWorkCycleCountMetadataKey] != wantCount {
			t.Fatalf("after cycle %d: cycle count = %q, want %q", i, got.Metadata[beadmeta.DrainAckAssignedWorkCycleCountMetadataKey], wantCount)
		}
	}
	if len(fake.Events) != 0 {
		t.Fatalf("events emitted before the cap tripped: %v", fake.Events)
	}
}

func TestEnforceDrainAckAssignedWorkCycleCap_TripsAndHoldsAtCap(t *testing.T) {
	env := newReconcilerTestEnv()
	fake := events.NewFake()
	env.rec = fake
	session, work := redispatchCapTestFixture(t, env)

	for i := 1; i < drainAckAssignedWorkCycleCap; i++ {
		enforceDrainAckAssignedWorkCycleCap("", env.cfg, env.store, nil, env.sessionInfo(session.ID), env.clk, env.rec, &env.stderr)
	}
	if len(fake.Events) != 0 {
		t.Fatalf("events emitted before the final (cap-tripping) cycle: %v", fake.Events)
	}

	// The cap-tripping cycle.
	enforceDrainAckAssignedWorkCycleCap("", env.cfg, env.store, nil, env.sessionInfo(session.ID), env.clk, env.rec, &env.stderr)

	got, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if got.Assignee != "" {
		t.Fatalf("assignee = %q, want cleared once the cap trips (ra-kuxm33 stayed blocked+assigned and still looped)", got.Assignee)
	}
	if !beadHasLabel(got, beadmeta.HoldMayorLabel) {
		t.Fatalf("labels = %v, want %s added", got.Labels, beadmeta.HoldMayorLabel)
	}
	if got.Metadata[beadmeta.DrainAckAssignedWorkCycleCountMetadataKey] != "" {
		t.Fatalf("cycle count = %q, want cleared after the auto-hold", got.Metadata[beadmeta.DrainAckAssignedWorkCycleCountMetadataKey])
	}
	if got.Metadata[beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey] != "" {
		t.Fatalf("cycle window start = %q, want cleared after the auto-hold", got.Metadata[beadmeta.DrainAckAssignedWorkCycleWindowStartMetadataKey])
	}

	var matched *events.Event
	matches := 0
	for i := range fake.Events {
		if fake.Events[i].Type == events.BeadRedispatchCapHeld {
			matches++
			matched = &fake.Events[i]
		}
	}
	if matches != 1 {
		t.Fatalf("%s events = %d, want exactly 1", events.BeadRedispatchCapHeld, matches)
	}
	if !strings.Contains(string(matched.Payload), work.ID) {
		t.Errorf("event payload does not reference work bead ID %q: %s", work.ID, matched.Payload)
	}
	if !strings.Contains(string(matched.Payload), "\"cycles\":"+strconv.Itoa(drainAckAssignedWorkCycleCap)) {
		t.Errorf("event payload does not carry cycles=%d: %s", drainAckAssignedWorkCycleCap, matched.Payload)
	}
}

// TestEnforceDrainAckAssignedWorkCycleCap_StaleWindowNeverTrips proves the cap
// is a per-window streak, not a lifetime total: a bead that drain-acks with
// assigned work occasionally over a long life (each observation outside
// drainAckAssignedWorkCycleWindow of the last) must never accumulate toward
// the cap, matching a healthy multi-turn item rather than a livelock.
func TestEnforceDrainAckAssignedWorkCycleCap_StaleWindowNeverTrips(t *testing.T) {
	env := newReconcilerTestEnv()
	fake := events.NewFake()
	env.rec = fake
	session, work := redispatchCapTestFixture(t, env)

	for i := 0; i < drainAckAssignedWorkCycleCap*3; i++ {
		enforceDrainAckAssignedWorkCycleCap("", env.cfg, env.store, nil, env.sessionInfo(session.ID), env.clk, env.rec, &env.stderr)
		env.clk.Advance(drainAckAssignedWorkCycleWindow + time.Minute)
	}

	got, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if got.Assignee != session.ID {
		t.Fatalf("assignee = %q, want unchanged — spaced-out cycles must never trip the cap", got.Assignee)
	}
	if beadHasLabel(got, beadmeta.HoldMayorLabel) {
		t.Fatalf("labels = %v, want no %s from spaced-out cycles", got.Labels, beadmeta.HoldMayorLabel)
	}
	if len(fake.Events) != 0 {
		t.Fatalf("events = %v, want none from spaced-out cycles", fake.Events)
	}
}

func beadHasLabel(b beads.Bead, label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// TestFinalizeDrainAckStoppedSession_WiresRedispatchCapCycle proves the
// production call site (finalizeDrainAckStoppedSession's hasAssignedWork
// branch) actually drives enforceDrainAckAssignedWorkCycleCap, not just a
// direct unit call to the guard itself.
func TestFinalizeDrainAckStoppedSession_WiresRedispatchCapCycle(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	fake := events.NewFake()
	env.rec = fake

	session := env.createSessionBead("worker", "worker")
	work, err := env.store.Create(beads.Bead{
		Title:    "implement phase work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}

	finalizeDrainAckStoppedSession(
		"", env.cfg, env.store, nil, env.sessionInfo(session.ID), "worker", false,
		newFakeDrainOps(), env.dt, env.clk, env.rec, &env.stderr,
	)

	got, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if got.Metadata[beadmeta.DrainAckAssignedWorkCycleCountMetadataKey] != "1" {
		t.Fatalf("cycle count = %q, want 1 after a single finalizeDrainAckStoppedSession call with assigned work", got.Metadata[beadmeta.DrainAckAssignedWorkCycleCountMetadataKey])
	}
	if got.Assignee != session.ID {
		t.Fatalf("assignee = %q, want unchanged (well below the cap on the first cycle)", got.Assignee)
	}
}
