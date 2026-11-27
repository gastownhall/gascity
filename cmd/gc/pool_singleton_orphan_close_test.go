package main

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
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

// cityTick runs one planner pass plus one reconcile pass, as the city loop
// does, and returns the planner pass's pool trace records.
func (e *singletonOrphanEnv) cityTick(t *testing.T, clk *clock.Fake) ([]SessionReconcilerTraceRecord, string) {
	t.Helper()
	var stderr bytes.Buffer
	tick := buildDesiredState(singletonOrphanCity, e.cityDir, clk.Now().UTC(), e.cfg, e.sp, e.store, &stderr)
	sessions, err := loadSessionBeads(e.store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	trace := newPoolDesiredStateTestTrace(e.cfg.Agents[0].QualifiedName())
	states := computePoolDesiredStates(e.cfg, tick.AssignedWorkBeads, sessionInfosFromBeads(sessions), tick.ScaleCheckCounts, nil, trace)
	poolDesired := PoolDesiredCounts(states)
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, tick.NamedSessionDemand, e.cfg)
	reconcileSessionBeadsAtPath(
		context.Background(), e.cityDir, sessions, tick.State, configuredSessionNames(e.cfg, singletonOrphanCity, e.store),
		e.cfg, e.sp, e.store, nil, tick.AssignedWorkBeads, nil, nil, newDrainTracker(), poolDesired,
		tick.StoreQueryPartial, nil, singletonOrphanCity,
		nil, clk, events.Discard, 0, 0, io.Discard, &stderr,
	)
	return trace.records, stderr.String()
}

// AC-2: with routed demand for the identity, the orphan pass that closes the
// seat (the planner has already dropped it from the desired set, as in the
// report) gives way to exactly one fresh seat under the alias within the
// next city ticks, and no later pool cap counts the closed row.
func TestCanonicalSingletonOrphanRecoversRoutedDemand(t *testing.T) {
	agent := singletonOrphanAgent()
	agent.ScaleCheck = printCountCommand(1)
	e := newSingletonOrphanEnv(t, agent)
	identity := e.cfg.Agents[0].QualifiedName()
	if _, err := e.store.Create(beads.Bead{
		Title:    "routed demand",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatalf("create routed demand: %v", err)
	}
	clk := &clock.Fake{Time: e.now}

	if records := e.tick(t); len(records) != 1 || records[0].OutcomeCode != TraceOutcomeClosed {
		t.Fatalf("tick 1 close_orphan outcomes = %v, want [closed]", closeOrphanOutcomes(records))
	}
	var stderrs []string
	for i := 2; i <= 3 && len(fakeStartNames(e.sp)) == 0; i++ {
		clk.Advance(time.Minute)
		records, stderr := e.cityTick(t, clk)
		stderrs = append(stderrs, stderr)
		for _, rec := range records {
			if rec.SiteCode != TraceSitePoolNewDemandCap {
				continue
			}
			for _, blocker := range poolTraceFieldStrings(t, rec.Fields, "blocking_sessions") {
				if blocker == e.seat.ID || blocker == e.seat.Metadata["session_name"] {
					t.Fatalf("tick %d: new_demand_cap lists the closed seat %q in blocking_sessions; stderr=%q", i, blocker, stderr)
				}
			}
		}
	}

	starts := fakeStartNames(e.sp)
	if len(starts) != 1 {
		t.Fatalf("session starts = %v, want exactly one; seat status=%q stderr=%q", starts, e.get(t, e.seat.ID).Status, stderrs)
	}
	if got := e.get(t, e.seat.ID).Status; got != "closed" {
		t.Fatalf("orphaned seat status = %q, want closed", got)
	}
	open, err := e.store.List(beads.ListQuery{Type: sessionBeadType})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var fresh []beads.Bead
	for _, b := range open {
		if b.Status != "closed" {
			fresh = append(fresh, b)
		}
	}
	if len(fresh) != 1 || fresh[0].ID == e.seat.ID {
		t.Fatalf("open session rows = %v, want one fresh row", fresh)
	}
	if got := fresh[0].Metadata["alias"]; got != identity {
		t.Fatalf("fresh seat alias = %q, want %q", got, identity)
	}
	if got := fresh[0].Metadata["session_name"]; got != starts[0] {
		t.Fatalf("started runtime %q is not the fresh seat's session_name %q", starts[0], got)
	}
	if got := e.get(t, e.claim.ID); got.Assignee != identity {
		t.Fatalf("alias claim assignee = %q, want kept on %q", got.Assignee, identity)
	}
}

// AC-5: a running canonical singleton seat working an in_progress claim under
// its alias is left alone: no drain, no close, no release.
func TestCanonicalSingletonRunningSeatKeepsItsInProgressClaim(t *testing.T) {
	agent := singletonOrphanAgent()
	agent.ScaleCheck = printCountCommand(0)
	e := newSingletonOrphanEnv(t, agent)
	identity := e.cfg.Agents[0].QualifiedName()
	working := "in_progress"
	if err := e.store.Update(e.claim.ID, beads.UpdateOpts{Status: &working}); err != nil {
		t.Fatalf("set claim in_progress: %v", err)
	}
	name := e.seat.Metadata["session_name"]
	if err := e.sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start seat runtime: %v", err)
	}
	if err := e.store.SetMetadataBatch(e.seat.ID, map[string]string{"state": "awake", "sleep_reason": "", "slept_at": ""}); err != nil {
		t.Fatalf("mark seat awake: %v", err)
	}
	clk := &clock.Fake{Time: e.now}

	for i := 1; i <= 3; i++ {
		_, stderr := e.cityTick(t, clk)
		seat := e.get(t, e.seat.ID)
		if seat.Status == "closed" {
			t.Fatalf("tick %d: running seat closed (close_reason=%q); stderr=%q", i, seat.Metadata["close_reason"], stderr)
		}
		if got := seat.Metadata["state"]; got != "awake" && got != "active" {
			t.Fatalf("tick %d: running seat state = %q, want it kept awake; stderr=%q", i, got, stderr)
		}
		if !e.sp.IsRunning(name) {
			t.Fatalf("tick %d: running seat's runtime stopped; stderr=%q", i, stderr)
		}
		if got := e.get(t, e.claim.ID); got.Assignee != identity || got.Status != working {
			t.Fatalf("tick %d: claim assignee=%q status=%q, want kept on %q and %s", i, got.Assignee, got.Status, identity, working)
		}
		clk.Advance(time.Minute)
	}
	if starts := fakeStartNames(e.sp); len(starts) != 1 {
		t.Fatalf("session starts = %v, want only the fixture's own start", starts)
	}
}

// AC-6: a max >= 2 pool's transient slot row carries no canonical identity,
// so in AC-1's orphaned state it keeps today's close_orphan outcome and the
// identity-preserving close never fires; a named session's row is the ga-n2d
// phantom path's, never this one's.
func TestCanonicalSingletonOrphanCloseSkipsTransientAndNamedRows(t *testing.T) {
	t.Run("transient slot", func(t *testing.T) {
		agent := singletonOrphanAgent()
		agent.MaxActiveSessions = intPtr(2)
		e := newSingletonOrphanEnv(t, agent)
		if _, ok := canonicalSingletonSeatIdentity(sessionInfoFromBead(e.seat), e.cfg); ok {
			t.Fatalf("transient slot row (alias %q) reports a canonical singleton identity", e.seat.Metadata["alias"])
		}

		records := e.tick(t)

		// Today the claim under the template identity is not the transient
		// row's, so the sweep closes the row before the orphan close sees it.
		for _, rec := range records {
			if _, ok := rec.Fields["preserved_identity"]; ok {
				t.Fatalf("close_orphan carries preserved_identity %v for a transient slot", rec.Fields["preserved_identity"])
			}
		}
		seat := e.get(t, e.seat.ID)
		if seat.Status != "closed" || seat.Metadata["state"] != "gc_swept" {
			t.Fatalf("transient row status=%q state=%q, want today's sweep (closed, gc_swept)", seat.Status, seat.Metadata["state"])
		}
		if got := e.get(t, e.claim.ID); got.Assignee != e.cfg.Agents[0].QualifiedName() || got.Status != "open" {
			t.Fatalf("claim assignee=%q status=%q, want untouched", got.Assignee, got.Status)
		}
	})
	t.Run("named session", func(t *testing.T) {
		e := newSingletonOrphanEnv(t, singletonOrphanAgent())
		info := sessionInfoFromBead(e.seat)
		info.ConfiguredNamedSession = true
		if identity, ok := canonicalSingletonSeatIdentity(info, e.cfg); ok {
			t.Fatalf("named session row reports canonical singleton identity %q", identity)
		}
	})
}
