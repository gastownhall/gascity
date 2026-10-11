package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// closeOrphanEnv is the shared fixture for the orphan-close tests: one pool
// agent, one session bead, and one open work bead routed to that pool and
// assigned to the seat.
type closeOrphanEnv struct {
	cityDir string
	cfg     *config.City
	store   beads.Store
	sp      *runtime.Fake
	tracer  *SessionReconcilerTracer
	session beads.Bead
	work    beads.Bead
}

func newCloseOrphanEnv(t *testing.T) *closeOrphanEnv {
	t.Helper()
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
	// Type + label matter: the reconciler's liveness probe resolves a seat
	// through session.ResolveSessionRecordByExactID, which only recognizes a
	// bead as a session record when it is typed (or labeled and untyped). An
	// untyped bead still reconciles — newSessionBeadSnapshot builds Info
	// directly — but every liveness observation on it reads not-running, which
	// silently turns a live seat into a confirmed orphan.
	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
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

	work, err := store.Create(beads.Bead{
		Title:    "assigned work",
		Status:   "open",
		Assignee: "polecat-1",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "repo/polecat",
		},
	})
	if err != nil {
		t.Fatalf("Create work bead: %v", err)
	}

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

	return &closeOrphanEnv{
		cityDir: cityDir,
		cfg:     cfg,
		store:   store,
		sp:      runtime.NewFake(),
		tracer:  tracer,
		session: sessionBead,
		work:    work,
	}
}

// tick runs one reconcile pass with the given desired state.
func (e *closeOrphanEnv) tick(t *testing.T, desired map[string]TemplateParams) []SessionReconcilerTraceRecord {
	t.Helper()
	armNow := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	cr := &CityRuntime{
		cityPath:            e.cityDir,
		cityName:            "trace-town",
		cfg:                 e.cfg,
		sp:                  e.sp,
		trace:               e.tracer,
		standaloneCityStore: e.store,
		sessionDrains:       newDrainTracker(),
		rec:                 events.NewFake(),
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	sessionBeads := newSessionBeadSnapshot([]beads.Bead{e.session})
	cycle := e.tracer.BeginCycle(TraceTickTriggerPatrol, "controller_tick", armNow, e.cfg)
	if cycle == nil {
		t.Fatal("BeginCycle returned nil")
	}
	cycle.configRevision = "rev-close-orphan-1"
	cycle.syncArms(armNow, e.cfg)

	if desired == nil {
		desired = map[string]TemplateParams{}
	}
	cr.beadReconcileTick(context.Background(), DesiredStateResult{
		State:             desired,
		AssignedWorkBeads: []beads.Bead{e.work},
	}, sessionBeads, cycle, false)
	if err := cycle.End(TraceCompletionCompleted, traceRecordPayload{"phase": "tick"}); err != nil {
		t.Fatalf("cycle.End: %v", err)
	}
	if err := e.tracer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	records, err := ReadTraceRecords(traceCityRuntimeDir(e.cityDir), TraceFilter{TraceID: cycle.traceID})
	if err != nil {
		t.Fatalf("ReadTraceRecords: %v", err)
	}
	return records
}

func (e *closeOrphanEnv) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	got, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return got
}

// TestSessionReconcilerOrphanCloseReleasesHeldWorkThenCloses pins the repair for
// the three-way deadlock in ga-jrnou.
//
// A confirmed-orphaned seat that still holds open assigned work could not be
// resolved by any lane: the close guard refuses while work is assigned
// (session_work_guard.go), the wake path is blocked because an orphaned base
// state raises BlockerMissingConfig, and releaseOrphanedPoolAssignments skips
// the work because the seat's session bead is still open
// (liveOpenSessionAssignmentExists tests bead status, not runtime liveness).
// Each lane correctly deferred to the others and the seat wedged indefinitely —
// 86 hours, in the maintainer-city case that surfaced it.
//
// The orphan close is the one site that has already confirmed the runtime is
// observably dead, so it owns the tie-break: release the held work, then close.
func TestSessionReconcilerOrphanCloseReleasesHeldWorkThenCloses(t *testing.T) {
	env := newCloseOrphanEnv(t)

	// Empty desired state and a never-started provider: the seat is a confirmed
	// orphan this tick.
	env.tick(t, nil)

	work := env.get(t, env.work.ID)
	if work.Assignee != "" {
		t.Fatalf("work assignee = %q, want empty — a confirmed-orphaned seat must not keep holding work", work.Assignee)
	}
	if work.Status != "open" {
		t.Fatalf("work status = %q, want open — released work must return to open so pool demand can re-route it", work.Status)
	}

	session := env.get(t, env.session.ID)
	if session.Status != "closed" {
		t.Fatalf("session bead status = %q, want closed — once the held work is released the close guard no longer refuses, so the orphan must close in the same tick", session.Status)
	}
}

// TestSessionReconcilerOrphanCloseRecordsClosedOnlyWhenClosed keeps the close
// sites honest about what they did. Both close sites recorded their decision
// BEFORE attempting the close and hard-coded TraceOutcomeClosed, so a seat that
// never closed was still reported "closed" every tick. gc trace is the
// documented entry point for a stuck reconciler
// (engdocs/contributors/reconciler-debugging.md), so that misreport actively
// misdirected the investigation for as long as the seat stayed wedged.
//
// Here the close DOES happen (the release above unblocks it), so the record
// must say closed — and it must say so because the bead closed, not because the
// site was reached.
func TestSessionReconcilerOrphanCloseRecordsClosedOnlyWhenClosed(t *testing.T) {
	env := newCloseOrphanEnv(t)
	records := env.tick(t, nil)

	session := env.get(t, env.session.ID)
	var sawCloseOrphan bool
	for _, rec := range records {
		if rec.SiteCode != TraceSiteReconcilerCloseOrphan {
			continue
		}
		sawCloseOrphan = true
		wantClosed := session.Status == "closed"
		gotClosed := rec.OutcomeCode == TraceOutcomeClosed
		if wantClosed != gotClosed {
			t.Fatalf("close_orphan outcome_code = %q but session bead %s is %q — the decision must report the close that actually happened, not one that was only attempted",
				rec.OutcomeCode, env.session.ID, session.Status)
		}
	}
	if !sawCloseOrphan {
		t.Fatalf("no %s decision record found in %d records — the orphan close path must be reached for this test to have teeth", TraceSiteReconcilerCloseOrphan, len(records))
	}
}

// TestSessionReconcilerOrphanCloseLeavesLiveSeatWorkAlone is the control for
// the release above, and the one that matters: releasing work from a seat that
// is actually alive is data loss, not recovery. The releaser this repair reuses
// carries that scar already (ga-g3pf0, where a session store serving zero
// session beads read as "every assignee is dead" and released live work every
// tick).
//
// Here the seat is running in the provider but absent from the desired state.
// That is the DRAIN path, not the orphan close — the runtime is observably
// alive, so nothing may touch its work.
func TestSessionReconcilerOrphanCloseLeavesLiveSeatWorkAlone(t *testing.T) {
	env := newCloseOrphanEnv(t)
	if err := env.sp.Start(context.Background(), "polecat-1", runtime.Config{}); err != nil {
		t.Fatalf("seed provider session: %v", err)
	}

	records := env.tick(t, nil)

	// Assert the mechanism, not just the outcome: the release only exists
	// inside the orphan close, so the control has teeth exactly as long as a
	// live seat never reaches that site.
	for _, rec := range records {
		if rec.SiteCode == TraceSiteReconcilerCloseOrphan {
			t.Fatalf("live seat reached %s (reason %q) — a running runtime must never be treated as an orphan", rec.SiteCode, rec.ReasonCode)
		}
	}

	work := env.get(t, env.work.ID)
	if work.Assignee != "polecat-1" {
		t.Fatalf("work assignee = %q, want polecat-1 — a seat whose runtime is observably ALIVE must keep its work; releasing it is data loss, not recovery", work.Assignee)
	}

	session := env.get(t, env.session.ID)
	if session.Status == "closed" {
		t.Fatalf("session bead closed, want still open — a live runtime takes the drain path, never the orphan close")
	}
}
