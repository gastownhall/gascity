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
	"github.com/gastownhall/gascity/internal/session"
)

// singletonOrphanEnv is issue #9245's wedge: a demand-only
// max_active_sessions = 1 pool whose one seat drain-acked asleep (idle) under
// the template alias, its runtime gone, absent from the desired set, holding
// a routeless claim assigned to that alias.
type singletonOrphanEnv struct {
	cityDir string
	cfg     *config.City
	store   beads.Store
	sp      *runtime.Fake
	tracer  *SessionReconcilerTracer
	seat    beads.Bead
	claim   beads.Bead
	now     time.Time
}

const singletonOrphanCity = "singleton-town"

// singletonOrphanAgent is the reporter's agent: rig-scoped, max one, no
// named session and no scale_check override.
func singletonOrphanAgent() config.Agent {
	return config.Agent{Name: "claude", Dir: "rig", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)}
}

func newSingletonOrphanEnv(t *testing.T, agent config.Agent) *singletonOrphanEnv {
	t.Helper()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, singletonOrphanCity, agent.Name)
	cfg := &config.City{
		Workspace: config.Workspace{Name: singletonOrphanCity},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents:    []config.Agent{agent},
	}
	store := beads.NewMemStore()
	seat := createCanonicalPoolSession(t, store, &cfg.Agents[0], now.Add(-time.Hour), 1)
	if err := store.SetMetadataBatch(seat.ID, map[string]string{
		"state":                     "asleep",
		"sleep_reason":              string(session.SleepReasonIdle),
		"slept_at":                  now.Add(-time.Minute).Format(time.RFC3339),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark seat asleep: %v", err)
	}
	e := &singletonOrphanEnv{cityDir: cityDir, cfg: cfg, store: store, sp: runtime.NewFake(), now: now}
	e.seat = e.get(t, seat.ID)
	e.claim = e.addClaim(t, "open", cfg.Agents[0].QualifiedName())

	e.tracer = newSessionReconcilerTracer(cityDir, singletonOrphanCity, io.Discard)
	if !e.tracer.Enabled() {
		t.Fatal("tracer should be enabled")
	}
	if _, err := e.tracer.armStore.upsertArm(TraceArm{
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     cfg.Agents[0].QualifiedName(),
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		ArmedAt:        now,
		ExpiresAt:      now.Add(15 * time.Minute),
		LastExtendedAt: now,
		UpdatedAt:      now,
	}); err != nil {
		t.Fatalf("upsert arm: %v", err)
	}
	return e
}

// addClaim creates a routeless work bead with status assigned to assignee.
func (e *singletonOrphanEnv) addClaim(t *testing.T, status, assignee string) beads.Bead {
	t.Helper()
	work, err := e.store.Create(beads.Bead{Title: "claim " + status, Type: "task", Status: "open", Assignee: assignee})
	if err != nil {
		t.Fatalf("create claim: %v", err)
	}
	if status != "open" {
		if err := e.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("set claim status %s: %v", status, err)
		}
	}
	return e.get(t, work.ID)
}

func (e *singletonOrphanEnv) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// tick runs one legacy reconcile pass over the seat with an empty desired
// set and returns the pass's close_orphan trace records.
func (e *singletonOrphanEnv) tick(t *testing.T) []SessionReconcilerTraceRecord {
	t.Helper()
	cr := &CityRuntime{
		cityPath:            e.cityDir,
		cityName:            singletonOrphanCity,
		cfg:                 e.cfg,
		sp:                  e.sp,
		trace:               e.tracer,
		standaloneCityStore: e.store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.NewFake(),
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	cycle := e.tracer.BeginCycle(TraceTickTriggerPatrol, "controller_tick", e.now, e.cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.syncArms(e.now, e.cfg)
	claims, err := e.store.List(beads.ListQuery{Type: "task"})
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), DesiredStateResult{
		State:             map[string]TemplateParams{},
		AssignedWorkBeads: claims,
	}, newSessionBeadSnapshot([]beads.Bead{e.get(t, e.seat.ID)}), cycle, false)
	if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"}); err != nil {
		t.Fatalf("cycle.End: %v", err)
	}
	if err := e.tracer.Close(); err != nil {
		t.Fatalf("tracer.Close: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(e.cityDir), TraceFilter{TraceID: cycle.traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	var out []SessionReconcilerTraceRecord
	for _, r := range records {
		if r.SiteCode == TraceSiteReconcilerCloseOrphan {
			out = append(out, r)
		}
	}
	return out
}

// closeOrphanOutcomes is records' outcome codes, for a failure message.
func closeOrphanOutcomes(records []SessionReconcilerTraceRecord) []TraceOutcomeCode {
	out := make([]TraceOutcomeCode, 0, len(records))
	for _, r := range records {
		out = append(out, r.OutcomeCode)
	}
	return out
}

// AC-1: the orphaned canonical singleton seat blocked only by claims under
// its alias closes, and those claims keep the alias for the next seat.
func TestReconcileSessionBeads_OrphanedCanonicalSingletonClosesKeepingIdentityClaims(t *testing.T) {
	e := newSingletonOrphanEnv(t, singletonOrphanAgent())
	identity := e.cfg.Agents[0].QualifiedName()
	if got := e.seat.Metadata["alias"]; got != identity {
		t.Fatalf("fixture seat alias = %q, want the template identity %q", got, identity)
	}

	records := e.tick(t)

	if len(records) != 1 {
		t.Fatalf("close_orphan outcomes = %v, want one", closeOrphanOutcomes(records))
	}
	if records[0].OutcomeCode != TraceOutcomeClosed {
		t.Fatalf("close_orphan outcome = %q, want %q", records[0].OutcomeCode, TraceOutcomeClosed)
	}
	if got := records[0].Fields["preserved_identity"]; got != identity {
		t.Fatalf("close_orphan preserved_identity = %v, want %q", got, identity)
	}
	if got := e.get(t, e.seat.ID).Status; got != "closed" {
		t.Fatalf("seat status = %q, want closed", got)
	}
	claim := e.get(t, e.claim.ID)
	if claim.Assignee != identity || claim.Status != "open" {
		t.Fatalf("claim assignee=%q status=%q, want kept on %q and open", claim.Assignee, claim.Status, identity)
	}
}

// AC-3: a claim under the seat's own identifiers (its bead ID or
// session_name) is the row's, so the identity-preserving close does not fire
// over it and the orphan close reports today's no_change.
func TestCanonicalSingletonOrphanCloseKeepsRowOwnedClaimsGated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		owner  func(seat beads.Bead) string
	}{
		{"open under bead id", "open", func(seat beads.Bead) string { return seat.ID }},
		{"in_progress under bead id", "in_progress", func(seat beads.Bead) string { return seat.ID }},
		{"open under session_name", "open", func(seat beads.Bead) string { return seat.Metadata["session_name"] }},
		{"in_progress under session_name", "in_progress", func(seat beads.Bead) string { return seat.Metadata["session_name"] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSingletonOrphanEnv(t, singletonOrphanAgent())
			owner := tc.owner(e.seat)
			if owner == "" {
				t.Fatal("fixture seat has no identifier for the row-owned claim")
			}
			own := e.addClaim(t, tc.status, owner)

			records := e.tick(t)

			if len(records) != 1 || records[0].OutcomeCode != TraceOutcomeNoChange {
				t.Fatalf("close_orphan outcomes = %v, want [no_change]", closeOrphanOutcomes(records))
			}
			if _, ok := records[0].Fields["preserved_identity"]; ok {
				t.Fatalf("close_orphan carries preserved_identity %v; the preserving close must not fire", records[0].Fields["preserved_identity"])
			}
			if got := e.get(t, e.seat.ID).Status; got == "closed" {
				t.Fatal("seat closed over a claim it owns under its own identifiers")
			}
			if got := e.get(t, own.ID); got.Assignee != owner || got.Status != tc.status {
				t.Fatalf("row-owned claim assignee=%q status=%q, want %q/%q", got.Assignee, got.Status, owner, tc.status)
			}
			if got := e.get(t, e.claim.ID); got.Assignee != e.cfg.Agents[0].QualifiedName() {
				t.Fatalf("alias claim assignee = %q, want kept on the alias", got.Assignee)
			}
		})
	}
}

// AC-4: an operator's suspend keeps the seat whole: the row stays open and
// its claims keep their assignee.
func TestCanonicalSingletonOrphanCloseHonorsOperatorHold(t *testing.T) {
	e := newSingletonOrphanEnv(t, singletonOrphanAgent())
	if err := e.store.SetMetadata(e.seat.ID, "sleep_intent", string(session.SleepReasonUserHold)); err != nil {
		t.Fatalf("hold seat: %v", err)
	}
	if identity, ok := canonicalSingletonSeatIdentity(sessionInfoFromBead(e.get(t, e.seat.ID)), e.cfg); ok {
		t.Fatalf("a held seat reports canonical singleton identity %q; an operator's suspend keeps its slot", identity)
	}

	records := e.tick(t)

	if len(records) != 1 || records[0].OutcomeCode != TraceOutcomeNoChange {
		t.Fatalf("close_orphan outcomes = %v, want [no_change]", closeOrphanOutcomes(records))
	}
	if got := e.get(t, e.seat.ID).Status; got == "closed" {
		t.Fatal("seat under an operator suspend closed")
	}
	if got := e.get(t, e.claim.ID); got.Assignee != e.cfg.Agents[0].QualifiedName() || got.Status != "open" {
		t.Fatalf("claim assignee=%q status=%q, want kept on the alias and open", got.Assignee, got.Status)
	}
}
