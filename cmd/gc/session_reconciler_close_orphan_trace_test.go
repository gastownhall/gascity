package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSessionReconcilerTraceCloseOrphanRecordsRefusalWhenWorkAssigned pins the
// close-orphan site's outcome to what actually happened.
//
// closeSessionBeadIfReachableStoreUnassigned refuses to close a session bead
// that still holds open assigned work (session_work_guard.go). That refusal is
// deliberate — but the decision record was emitted BEFORE the close was
// attempted and hard-coded TraceOutcomeClosed, so a seat that never closed was
// reported as "closed" on every tick. `gc trace` is the documented tool for
// diagnosing a stuck reconciler (engdocs/contributors/reconciler-debugging.md);
// a site that reports success for a close it did not perform sends that
// investigation the wrong way for as long as the seat stays wedged.
//
// Scenario: an orphan seat (not desired, not running, liveness observable) that
// still has an open work bead assigned to it. The close is refused, so the
// record must not claim "closed".
func TestSessionReconcilerTraceCloseOrphanRecordsRefusalWhenWorkAssigned(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "trace-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{
			{
				Name:              "polecat",
				Dir:               "repo",
				MinActiveSessions: intPtr(0),
				MaxActiveSessions: intPtr(1),
			},
		},
	}

	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title: "polecat",
		Metadata: map[string]string{
			"session_name":       "polecat-1",
			"template":           "repo/polecat",
			"agent_name":         "polecat",
			"state":              "asleep",
			"generation":         "1",
			"continuation_epoch": "1",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	// The work that makes the close guard refuse. Without this the orphan closes
	// normally and the assertion below would be vacuous.
	if _, err := store.Create(beads.Bead{
		Title:    "assigned work",
		Status:   "open",
		Assignee: "polecat-1",
	}); err != nil {
		t.Fatalf("Create work bead: %v", err)
	}

	// Never started: the seat is not running, and the fake provider can still
	// observe that (no liveness error), so the reconciler reaches the close.
	sp := runtime.NewFake()

	tracer := newSessionReconcilerTracer(cityDir, "trace-town", io.Discard)
	if !tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	armNow := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	if _, err := tracer.armStore.upsertArm(TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		ArmedAt:        armNow,
		ExpiresAt:      armNow.Add(15 * time.Minute),
		LastExtendedAt: armNow,
		UpdatedAt:      armNow,
	}); err != nil {
		t.Fatalf("upsert arm: %v", err)
	}

	cr := &CityRuntime{
		cityPath:            cityDir,
		cityName:            "trace-town",
		cfg:                 cfg,
		sp:                  sp,
		trace:               tracer,
		standaloneCityStore: store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.NewFake(),
		stdout:              io.Discard,
		stderr:              io.Discard,
	}

	sessionBeads := newSessionBeadSnapshot([]beads.Bead{sessionBead})
	cycle := tracer.BeginCycle(TraceTickTriggerPatrol, "controller_tick", armNow, cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.configRevision = "rev-close-orphan-1"
	cycle.syncArms(armNow, cfg)

	// Empty desired state: the seat is an orphan this tick.
	cr.beadReconcileTick(context.Background(), DesiredStateResult{State: map[string]TemplateParams{}}, sessionBeads, cycle, false)
	if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"}); err != nil {
		t.Fatalf("cycle.End: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Control: the close must actually have been refused. If the guard let it
	// through, this scenario proves nothing about the outcome code.
	got, err := store.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", sessionBead.ID, err)
	}
	if got.Status == "closed" {
		t.Fatalf("session bead status = closed, want still open — the close guard must refuse while open work is assigned, otherwise this test cannot exercise the refusal outcome")
	}

	records, err := ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{TraceID: cycle.traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var sawCloseOrphan bool
	for _, rec := range records {
		if rec.SiteCode != TraceSiteReconcilerCloseOrphan {
			continue
		}
		sawCloseOrphan = true
		if rec.OutcomeCode == TraceOutcomeClosed {
			t.Fatalf("close_orphan outcome_code = %q, but session bead %s is still %q — the decision must report the close that actually happened, not one that was only attempted",
				rec.OutcomeCode, sessionBead.ID, got.Status)
		}
	}
	if !sawCloseOrphan {
		t.Fatalf("no %s decision record found in %d records — the orphan close path must be reached for this test to have teeth", TraceSiteReconcilerCloseOrphan, len(records))
	}
}
