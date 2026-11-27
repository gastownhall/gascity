package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
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
	stdout  bytes.Buffer // what tick's reconcile pass printed for operators
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
	return createWork(t, e.store, status, assignee, nil)
}

func (e *singletonOrphanEnv) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// park blocks the claim id behind a fresh open blocker, as a continuation
// pin waits on its predecessor, and returns the blocker.
func (e *singletonOrphanEnv) park(t *testing.T, id string) beads.Bead {
	t.Helper()
	blocker, err := e.store.Create(beads.Bead{Title: "review loop", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if err := e.store.DepAdd(id, blocker.ID, "blocks"); err != nil {
		t.Fatalf("park claim %s: %v", id, err)
	}
	return blocker
}

// addRoutedDemand creates open, unassigned work routed to the identity.
func (e *singletonOrphanEnv) addRoutedDemand(t *testing.T) {
	t.Helper()
	if _, err := e.store.Create(beads.Bead{
		Title:    "routed demand",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: e.cfg.Agents[0].QualifiedName()},
	}); err != nil {
		t.Fatalf("create routed demand: %v", err)
	}
}

// assertGivenBack fails unless the claim id went back to the pool as the
// close cascade gives back a routeless claim: open, unassigned and carrying
// the seat's route, which route recovery restores as its gc.routed_to.
func (e *singletonOrphanEnv) assertGivenBack(t *testing.T, id string) {
	t.Helper()
	identity := e.cfg.Agents[0].QualifiedName()
	got := e.get(t, id)
	if got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.RunTargetMetadataKey] != identity {
		t.Fatalf("claim %s status=%q assignee=%q gc.run_target=%q, want open and unassigned with the seat's route %q",
			id, got.Status, got.Assignee, got.Metadata[beadmeta.RunTargetMetadataKey], identity)
	}
	if route := carriedPoolRoute(got); route != identity {
		t.Fatalf("carriedPoolRoute(claim %s) = %q, want route recovery to restore %q", id, route, identity)
	}
}

// recoverRoute is the controller's route-recovery lane on one claim
// (routeRecoveryLane.restoreRoute): it carries the route the claim declares
// into gc.routed_to, where pool demand reads it.
func (e *singletonOrphanEnv) recoverRoute(t *testing.T, id string) {
	t.Helper()
	route := carriedPoolRoute(e.get(t, id))
	if route == "" {
		t.Fatalf("claim %s carries no route for route recovery to restore", id)
	}
	if err := e.store.SetMetadata(id, beadmeta.RoutedToMetadataKey, route); err != nil {
		t.Fatalf("restore route on %s: %v", id, err)
	}
}

// openSeats is every session row that is not closed.
func (e *singletonOrphanEnv) openSeats(t *testing.T) []beads.Bead {
	t.Helper()
	rows, err := e.store.List(beads.ListQuery{Type: sessionBeadType})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var open []beads.Bead
	for _, b := range rows {
		if b.Status != "closed" {
			open = append(open, b)
		}
	}
	return open
}

// assertOneFreshSeat fails unless exactly one runtime started, as the one
// open session row, a fresh one under the alias.
func (e *singletonOrphanEnv) assertOneFreshSeat(t *testing.T, stderrs []string) {
	t.Helper()
	identity := e.cfg.Agents[0].QualifiedName()
	starts := fakeStartNames(e.sp)
	if len(starts) != 1 {
		t.Fatalf("session starts = %v, want exactly one; seat status=%q stderr=%q", starts, e.get(t, e.seat.ID).Status, stderrs)
	}
	if got := e.get(t, e.seat.ID).Status; got != "closed" {
		t.Fatalf("orphaned seat status = %q, want closed", got)
	}
	fresh := e.openSeats(t)
	if len(fresh) != 1 || fresh[0].ID == e.seat.ID {
		t.Fatalf("open session rows = %v, want one fresh row", fresh)
	}
	if got := fresh[0].Metadata["alias"]; got != identity {
		t.Fatalf("fresh seat alias = %q, want %q", got, identity)
	}
	if got := fresh[0].Metadata["session_name"]; got != starts[0] {
		t.Fatalf("started runtime %q is not the fresh seat's session_name %q", starts[0], got)
	}
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
		stdout:              &e.stdout,
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

// AC-1: the orphaned canonical singleton seat blocked only by a claim under
// its alias closes, and the claim goes back to the pool with the seat's
// route. Kept on the alias, the routeless claim would be demand for nothing
// once the seat is gone. The close is recorded for operators: a trace field
// naming the identity and a line on the controller's output.
func TestReconcileSessionBeads_OrphanedCanonicalSingletonClosesGivingBackAliasClaims(t *testing.T) {
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
	if got := records[0].Fields["canonical_identity"]; got != identity {
		t.Fatalf("close_orphan canonical_identity = %v, want %q", got, identity)
	}
	if got := e.get(t, e.seat.ID).Status; got != "closed" {
		t.Fatalf("seat status = %q, want closed", got)
	}
	e.assertGivenBack(t, e.claim.ID)
	if out := e.stdout.String(); !strings.Contains(out, "Closed orphaned canonical singleton seat") || !strings.Contains(out, identity) {
		t.Fatalf("controller output = %q, want a line naming the closed seat's identity %q", out, identity)
	}
}

// AC-1 with a floor: a min_active_sessions = 1 singleton's orphaned seat
// closes the same way, giving back its alias claim, since the floor is met by
// a fresh seat under the alias, not by this dead row.
func TestReconcileSessionBeads_OrphanedCanonicalSingletonWithFloorCloses(t *testing.T) {
	agent := singletonOrphanAgent()
	agent.MinActiveSessions = intPtr(1)
	e := newSingletonOrphanEnv(t, agent)
	identity := e.cfg.Agents[0].QualifiedName()

	records := e.tick(t)

	if len(records) != 1 || records[0].OutcomeCode != TraceOutcomeClosed {
		t.Fatalf("close_orphan outcomes = %v, want [closed]", closeOrphanOutcomes(records))
	}
	if got := records[0].Fields["canonical_identity"]; got != identity {
		t.Fatalf("close_orphan canonical_identity = %v, want %q", got, identity)
	}
	if got := e.get(t, e.seat.ID).Status; got != "closed" {
		t.Fatalf("seat status = %q, want closed", got)
	}
	e.assertGivenBack(t, e.claim.ID)
}

// A claim routed to the identity that the orphan release keeps, because its
// detached runtime may still run it, stays on the alias when the seat closes:
// the pool's demand for the identity reaches it (the dead-assignee sweep
// probes it again), and the cascade does not release it unprobed.
func TestCanonicalSingletonOrphanCloseLeavesAHeldDetachedClaimOnTheAlias(t *testing.T) {
	resetDetachedProbeHoldLogForTest()
	t.Cleanup(resetDetachedProbeHoldLogForTest)
	e := newSingletonOrphanEnv(t, singletonOrphanAgent())
	identity := e.cfg.Agents[0].QualifiedName()
	spec := "tmux:" + filepath.Join(t.TempDir(), "gone") + ":soak-loop"
	held := createWork(t, e.store, "in_progress", identity, map[string]string{
		beadmeta.RoutedToMetadataKey: identity,
		detachedProbeMetadataKey:     spec,
	})

	records := e.tick(t)

	if len(records) != 1 || records[0].OutcomeCode != TraceOutcomeClosed {
		t.Fatalf("close_orphan outcomes = %v, want [closed]", closeOrphanOutcomes(records))
	}
	if got := e.get(t, held.ID); got.Assignee != identity || got.Status != "in_progress" || got.Metadata[detachedProbeMetadataKey] != spec {
		t.Fatalf("detached claim assignee=%q status=%q gc.detached=%q, want it held on %q with its probe",
			got.Assignee, got.Status, got.Metadata[detachedProbeMetadataKey], identity)
	}
	e.assertGivenBack(t, e.claim.ID)
}

// AC-3: a claim under the seat's own identifiers (its bead ID or
// session_name) is the row's, so the canonical singleton close does not fire
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
			if _, ok := records[0].Fields["canonical_identity"]; ok {
				t.Fatalf("close_orphan carries canonical_identity %v; the canonical singleton close must not fire", records[0].Fields["canonical_identity"])
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

// O5 for the canonical singleton close: a claim under the row's own bead ID
// that no lane serves does not keep the orphaned seat open, as it keeps no
// orphan's, beside a claim under the alias too. The seat closes, the claim
// stays on the closed row stamped stranded, and the alias claim goes back
// with the seat's route.
func TestCanonicalSingletonOrphanClosesOverRowOwnedClaimsNoLaneServes(t *testing.T) {
	e := newSingletonOrphanEnv(t, singletonOrphanAgent())
	identity := e.cfg.Agents[0].QualifiedName()
	own := createWork(t, e.store, "open", e.seat.ID, map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})

	records := e.tick(t)

	if len(records) != 1 || records[0].OutcomeCode != TraceOutcomeClosed {
		t.Fatalf("close_orphan outcomes = %v, want [closed]", closeOrphanOutcomes(records))
	}
	if got := records[0].Fields["canonical_identity"]; got != identity {
		t.Fatalf("close_orphan canonical_identity = %v, want %q", got, identity)
	}
	got := e.get(t, own.ID)
	if got.Assignee != e.seat.ID || got.Status != "open" || got.Metadata[beadmeta.StrandedMetadataKey] != "true" || got.Metadata[beadmeta.StrandedSessionMetadataKey] != e.seat.ID {
		t.Fatalf("row-owned claim assignee=%q status=%q gc.stranded=%q gc.stranded_session=%q, want it kept on the closed row %s, stamped stranded",
			got.Assignee, got.Status, got.Metadata[beadmeta.StrandedMetadataKey], got.Metadata[beadmeta.StrandedSessionMetadataKey], e.seat.ID)
	}
	e.assertGivenBack(t, e.claim.ID)
}

// rowOwnsNoServedWorkBesidesIdentity reads the row's own work twice, as a
// work-gated close does: a claim assigned to the row after the tick's read
// still holds it (the close-time re-read), and a work leg that cannot be read
// holds it too (a failed read counts as work). A claim no lane serves does
// not hold it, on both reads, while one a lane serves beside it still does.
func TestRowOwnsNoServedWorkBesidesIdentityRereadsAndFailsClosed(t *testing.T) {
	t.Run("a claim assigned after the tick's read", func(t *testing.T) {
		e := newSingletonOrphanEnv(t, singletonOrphanAgent())
		identity := e.cfg.Agents[0].QualifiedName()
		info := sessionInfoFromBead(e.seat)
		sw := testSeatWork("", e.cfg, e.store, nil)
		if !rowOwnsNoServedWorkBesidesIdentity(sw, info, e.cfg, identity) {
			t.Fatal("before the late claim: the row owns work besides its alias claim, want none")
		}
		late := e.addClaim(t, "open", e.seat.ID)
		if held, err := seatHasWorkForCloseGate(sw, rowOwnScope(info, e.cfg, identity)); err != nil || held {
			t.Fatalf("the tick's read reports held=%v err=%v, want it to predate claim %s", held, err, late.ID)
		}
		if rowOwnsNoServedWorkBesidesIdentity(sw, info, e.cfg, identity) {
			t.Fatalf("claim %s assigned to the row after the tick's read does not hold it", late.ID)
		}
	})
	t.Run("a work leg that cannot be read", func(t *testing.T) {
		e := newSingletonOrphanEnv(t, singletonOrphanAgent())
		info := sessionInfoFromBead(e.seat)
		sw := testSeatWork("", e.cfg, failingListStore{Store: e.store}, nil)
		if rowOwnsNoServedWorkBesidesIdentity(sw, info, e.cfg, e.cfg.Agents[0].QualifiedName()) {
			t.Fatal("a dark work leg reads as no work; a failed read must count as work")
		}
	})
	t.Run("a claim no lane serves", func(t *testing.T) {
		e := newSingletonOrphanEnv(t, singletonOrphanAgent())
		identity := e.cfg.Agents[0].QualifiedName()
		info := sessionInfoFromBead(e.seat)
		unserved := createWork(t, e.store, "in_progress", e.seat.ID, map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		if !rowOwnsNoServedWorkBesidesIdentity(testSeatWork("", e.cfg, e.store, nil), info, e.cfg, identity) {
			t.Fatalf("claim %s, which no lane serves, holds the row; it stays on the closed row (O5)", unserved.ID)
		}
		served := e.addClaim(t, "open", e.seat.ID)
		if rowOwnsNoServedWorkBesidesIdentity(testSeatWork("", e.cfg, e.store, nil), info, e.cfg, identity) {
			t.Fatalf("claim %s, which the seat's route serves, does not hold the row beside unserved claim %s", served.ID, unserved.ID)
		}
	})
}

// The cascade of a canonical singleton seat's orphan close keeps, of the
// claims its release scope holds, only those held under the identity and
// routed to it, and reports each one. A routeless alias claim, an alias claim
// routed to another pool and the row's own claim go back as the cascade gives
// back any claim. An expanded workflow root under the alias routes nowhere
// (#5900): it stays as a claim no lane serves, reported as such.
func TestCanonicalIdentityCascadeKeepsOnlyClaimsRoutedToTheIdentity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: singletonOrphanCity},
		Agents: []config.Agent{
			singletonOrphanAgent(),
			{Name: "other", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(3)},
		},
	}
	store := beads.NewMemStore()
	seat := createCanonicalPoolSession(t, store, &cfg.Agents[0], time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC), 1)
	identity, other := cfg.Agents[0].QualifiedName(), cfg.Agents[1].QualifiedName()
	routed := createWork(t, store, "in_progress", identity, map[string]string{beadmeta.RoutedToMetadataKey: identity})
	routeless := createWork(t, store, "open", identity, nil)
	elsewhere := createWork(t, store, "open", identity, map[string]string{beadmeta.RoutedToMetadataKey: other})
	own := createWork(t, store, "in_progress", seat.ID, nil)
	expanded := createWork(t, store, "open", identity, map[string]string{
		beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
		beadmeta.RunTargetMetadataKey:        identity,
		beadmeta.WorkflowExpandedMetadataKey: "true",
	})

	var stderr bytes.Buffer
	out := releaseWorkFromClosedSessionBeadExcept(store, testSeatWork("", cfg, store, nil), seat, nil, identity, &stderr)

	if out.Why[routed.ID] != keptIdentity {
		t.Fatalf("claim routed to the identity: why=%q, want %q; outcome %+v", out.Why[routed.ID], keptIdentity, out)
	}
	if unserved := out.Unserved(); out.Why[expanded.ID] != keptUnservedRoute || len(unserved) != 1 || unserved[0].id != expanded.ID {
		t.Fatalf("expanded workflow root: why=%q, unserved %+v, want it kept as the one claim no lane serves", out.Why[expanded.ID], unserved)
	}
	assertWork(t, store, expanded.ID, "open", identity)
	assertWork(t, store, routed.ID, "in_progress", identity)
	if !strings.Contains(stderr.String(), "leaves claim "+routed.ID+" with identity") {
		t.Fatalf("stderr = %q, want the claim left with the identity reported", stderr.String())
	}
	for _, id := range []string{routeless.ID, own.ID} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.RunTargetMetadataKey] != identity {
			t.Fatalf("claim %s status=%q assignee=%q gc.run_target=%q, want it given back with the seat's route %q",
				id, got.Status, got.Assignee, got.Metadata[beadmeta.RunTargetMetadataKey], identity)
		}
	}
	got, err := store.Get(elsewhere.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.Metadata[beadmeta.RoutedToMetadataKey] != other || got.Metadata[beadmeta.RunTargetMetadataKey] != "" {
		t.Fatalf("claim routed to %q: assignee=%q gc.routed_to=%q gc.run_target=%q, want it given back on its own route",
			other, got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], got.Metadata[beadmeta.RunTargetMetadataKey])
	}
}

// canonicalIdentityKeeps is the cascade's whole rule: a claim stays with the
// identity only when it is held under the identity and routes to it.
func TestCanonicalIdentityKeeps(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{singletonOrphanAgent(), {Name: "other", Dir: "rig", StartCommand: "true", MaxActiveSessions: intPtr(3)}}}
	identity := cfg.Agents[0].QualifiedName()
	for _, tc := range []struct {
		name     string
		identity string
		bead     beads.Bead
		want     bool
	}{
		{"routed to the identity", identity, beads.Bead{Assignee: identity, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: identity}}, true},
		{"a workflow root's run_target", identity, beads.Bead{Assignee: identity, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RunTargetMetadataKey: identity}}, true},
		{"an expanded workflow root's run_target", identity, beads.Bead{Assignee: identity, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RunTargetMetadataKey: identity, beadmeta.WorkflowExpandedMetadataKey: "true"}}, false},
		{"routeless", identity, beads.Bead{Assignee: identity}, false},
		{"a plain bead's run_target alone", identity, beads.Bead{Assignee: identity, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: identity}}, false},
		{"routed to another pool", identity, beads.Bead{Assignee: identity, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: cfg.Agents[1].QualifiedName()}}, false},
		{"held under another identifier", identity, beads.Bead{Assignee: "gc-1", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: identity}}, false},
		{"no identity", "", beads.Bead{Assignee: identity, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: identity}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalIdentityKeeps(cfg, tc.identity, tc.bead); got != tc.want {
				t.Fatalf("canonicalIdentityKeeps = %v, want %v", got, tc.want)
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

// The wedge on base (issue #9245), driven by the planner alone: a blocked
// routeless claim under the alias, plus routed demand for the identity. The
// asleep seat is not desired, so each tick takes the orphan close, which used
// to refuse over the alias claim while the routed demand could not take the
// alias the seat still held. Now the seat closes giving the claim back, and
// exactly one fresh seat starts under the alias within two ticks.
func TestCanonicalSingletonOrphanUnwedgesThroughThePlanner(t *testing.T) {
	agent := singletonOrphanAgent()
	agent.ScaleCheck = printCountCommand(1)
	e := newSingletonOrphanEnv(t, agent)
	e.park(t, e.claim.ID)
	e.addRoutedDemand(t)
	clk := &clock.Fake{Time: e.now}

	var stderrs []string
	for i := 1; i <= 2 && len(fakeStartNames(e.sp)) == 0; i++ {
		_, stderr := e.cityTick(t, clk)
		stderrs = append(stderrs, stderr)
		clk.Advance(time.Minute)
	}

	e.assertOneFreshSeat(t, stderrs)
	e.assertGivenBack(t, e.claim.ID)
}

// AC-2, the forced-state case: the tick that closes the seat forces the
// empty desired set the report shows, with routed demand for the identity.
// The close gives way to exactly one fresh seat under the alias within the
// next city ticks, no later pool cap counts the closed row, and the alias
// claim waits in the pool's queue with the seat's route.
// TestCanonicalSingletonOrphanUnwedgesThroughThePlanner drives the same wedge
// from the planner alone.
func TestCanonicalSingletonOrphanRecoversRoutedDemand(t *testing.T) {
	agent := singletonOrphanAgent()
	agent.ScaleCheck = printCountCommand(1)
	e := newSingletonOrphanEnv(t, agent)
	e.addRoutedDemand(t)
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

	e.assertOneFreshSeat(t, stderrs)
	e.assertGivenBack(t, e.claim.ID)
}

// AC-2 without demand: the close is the orphan's own resolution, not a
// reaction to demand. With the alias claim parked behind an open blocker and
// nothing routed to the identity, the seat still closes and the following
// city ticks open no fresh row and start no runtime.
func TestCanonicalSingletonOrphanCloseWithoutDemandStartsNothing(t *testing.T) {
	e := newSingletonOrphanEnv(t, singletonOrphanAgent())
	e.park(t, e.claim.ID)
	clk := &clock.Fake{Time: e.now}

	if records := e.tick(t); len(records) != 1 || records[0].OutcomeCode != TraceOutcomeClosed {
		t.Fatalf("close_orphan outcomes = %v, want [closed]", closeOrphanOutcomes(records))
	}
	var stderrs []string
	for i := 2; i <= 3; i++ {
		clk.Advance(time.Minute)
		_, stderr := e.cityTick(t, clk)
		stderrs = append(stderrs, stderr)
	}

	if starts := fakeStartNames(e.sp); len(starts) != 0 {
		t.Fatalf("session starts = %v, want none without demand; stderr=%q", starts, stderrs)
	}
	if open := e.openSeats(t); len(open) != 0 {
		t.Fatalf("open session rows %v after the close, want none without demand", open)
	}
	e.assertGivenBack(t, e.claim.ID)
}

// The blocked alias claim the close gave back is not lost once its blocker
// closes: route recovery restores the seat's route as its gc.routed_to, and
// the pool's own demand, with no routed work besides it, starts exactly one
// seat under the alias for it. Kept on the alias with no route instead, the
// claim would sit open with no seat, no demand and no stranded alert.
func TestCanonicalSingletonOrphanGivesABlockedAliasClaimAWayBack(t *testing.T) {
	e := newSingletonOrphanEnv(t, singletonOrphanAgent())
	identity := e.cfg.Agents[0].QualifiedName()
	blocker := e.park(t, e.claim.ID)
	clk := &clock.Fake{Time: e.now}

	if records := e.tick(t); len(records) != 1 || records[0].OutcomeCode != TraceOutcomeClosed {
		t.Fatalf("close_orphan outcomes = %v, want [closed]", closeOrphanOutcomes(records))
	}
	e.assertGivenBack(t, e.claim.ID)
	e.recoverRoute(t, e.claim.ID)
	clk.Advance(time.Minute)
	if _, stderr := e.cityTick(t, clk); len(fakeStartNames(e.sp)) != 0 {
		t.Fatalf("session starts = %v while the claim is blocked, want none; stderr=%q", fakeStartNames(e.sp), stderr)
	}

	if err := e.store.Close(blocker.ID); err != nil {
		t.Fatalf("close blocker: %v", err)
	}
	var stderrs []string
	for i := 0; i < 3 && len(fakeStartNames(e.sp)) == 0; i++ {
		clk.Advance(time.Minute)
		_, stderr := e.cityTick(t, clk)
		stderrs = append(stderrs, stderr)
	}

	e.assertOneFreshSeat(t, stderrs)
	if got := e.get(t, e.claim.ID); got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.RoutedToMetadataKey] != identity {
		t.Fatalf("claim status=%q assignee=%q gc.routed_to=%q, want it open in the %q queue for the fresh seat",
			got.Status, got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey], identity)
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
// canonical singleton close never fires; a named session's row is the ga-n2d
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
			if _, ok := rec.Fields["canonical_identity"]; ok {
				t.Fatalf("close_orphan carries canonical_identity %v for a transient slot", rec.Fields["canonical_identity"])
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

// AC-7: the v2 allocator closes AC-1's seat as legacy does. The row closes
// over the claims under its alias and gives them back: the routeless one
// through the cascade with the seat's route, a routed one through the orphan
// release before the close, as legacy's ga-jrnou release returns it. A routed
// claim the orphan release holds for its detached runtime stays on the alias.
// A claim under the row's own bead ID still refuses the close (AC-3's rule),
// unless no lane serves it: that one stays on the closed row, stamped
// stranded, as any orphan's does (O5). The cascade names the identity and the
// claims it gave back and kept, and no line reports a claim it gives back as
// kept. An agent's suspend closes the row as legacy's orphaned close does,
// and a city suspend keeps it.
func TestV2CloseOrphanedCanonicalSingletonGivesBackIdentityClaims(t *testing.T) {
	seat := func(t *testing.T) *closeCase {
		c := newCloseCase(t, livenessGone, desireDrain,
			"template", "rig/claude", "agent_name", "rig/claude", "alias", "rig/claude",
			"state", "asleep", "sleep_reason", string(session.SleepReasonIdle), "pool_slot", "")
		c.entry().DrainReason, c.entry().Template = drainOrphaned, "rig/claude"
		c.cfg.Agents = append(c.cfg.Agents, singletonOrphanAgent())
		c.w.Env = &reconcileEnv{Cfg: c.cfg}
		return c
	}
	closes := func(t *testing.T, c *closeCase) {
		t.Helper()
		it := c.decide()
		if it.Reason != decideCloseOrphan || it.Closing.Phantom || it.Closing.Identity != "rig/claude" || len(it.Closing.Preserve) != 0 {
			t.Fatalf("decideRow = %q closing %+v, want an orphan close of the rig/claude identity's seat", it.Reason, it.Closing)
		}
		if s := c.run(t, gone().tmux()); s.Outcome != settledLanded || !s.Closed {
			t.Fatalf("settlement %+v, want the close landed over the identity's claims", s)
		}
		if b := c.row(t); b.Status != "closed" {
			t.Fatalf("row status = %q, want closed", b.Status)
		}
	}
	givenBack := func(t *testing.T, c *closeCase, id string) {
		t.Helper()
		got, err := c.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.RunTargetMetadataKey] != "rig/claude" {
			t.Fatalf("alias claim status=%q assignee=%q gc.run_target=%q, want it given back with the seat's route",
				got.Status, got.Assignee, got.Metadata[beadmeta.RunTargetMetadataKey])
		}
	}
	// stamped fails unless the row's own claim id, which no lane serves,
	// stayed on the closed row, stamped stranded, and the close alerted on it.
	stamped := func(t *testing.T, c *closeCase, id string) {
		t.Helper()
		got, err := c.store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Assignee != c.k.ID || got.Status != "open" || got.Metadata[beadmeta.StrandedMetadataKey] != "true" || got.Metadata[beadmeta.StrandedSessionMetadataKey] != c.k.ID {
			t.Fatalf("row-owned claim assignee=%q status=%q gc.stranded=%q gc.stranded_session=%q, want it kept on the closed row %s, stamped stranded",
				got.Assignee, got.Status, got.Metadata[beadmeta.StrandedMetadataKey], got.Metadata[beadmeta.StrandedSessionMetadataKey], c.k.ID)
		}
		if evts, _ := c.rec.List(events.Filter{Type: events.SessionStrandedClaims}); len(evts) != 1 {
			t.Fatalf("session.stranded_claims = %d, want 1", len(evts))
		}
	}
	// reported fails unless the close logged its one canonical singleton line
	// as want, and no line that keeps claim given back.
	reported := func(t *testing.T, c *closeCase, want, givenBack string) {
		t.Helper()
		logged := c.stderr.String()
		if !strings.Contains(logged, want+"\n") {
			t.Fatalf("close logged %q, want the line %q", logged, want)
		}
		if strings.Contains(logged, "keeps claim "+givenBack+":") {
			t.Fatalf("close logged %q, which reports claim %s kept; the cascade gave it back", logged, givenBack)
		}
	}
	t.Run("a routeless alias claim goes back with the seat's route", func(t *testing.T) {
		c := seat(t)
		claim := createWork(t, c.store, "open", "rig/claude", nil)
		closes(t, c)
		givenBack(t, c, claim.ID)
		reported(t, c, fmt.Sprintf(`session beads: closed orphaned canonical singleton seat %s (identity "rig/claude"): gave back [%s], kept []`, c.k.ID, claim.ID), claim.ID)
	})
	t.Run("a claim the row owns that no lane serves does not keep it (O5)", func(t *testing.T) {
		c := seat(t)
		own := c.work(t, "open", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		closes(t, c)
		stamped(t, c, own.ID)
	})
	t.Run("nor does one beside a routeless alias claim, which goes back", func(t *testing.T) {
		c := seat(t)
		own := c.work(t, "open", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
		claim := createWork(t, c.store, "open", "rig/claude", nil)
		closes(t, c)
		stamped(t, c, own.ID)
		givenBack(t, c, claim.ID)
		reported(t, c, fmt.Sprintf(`session beads: closed orphaned canonical singleton seat %s (identity "rig/claude"): gave back [%s], kept [%s]`, c.k.ID, claim.ID, own.ID), claim.ID)
	})
	t.Run("a routed alias claim goes back through the orphan release", func(t *testing.T) {
		c := seat(t)
		claim := createWork(t, c.store, "in_progress", "rig/claude", map[string]string{beadmeta.RoutedToMetadataKey: "rig/claude"})
		closes(t, c)
		got, err := c.store.Get(claim.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.RoutedToMetadataKey] != "rig/claude" {
			t.Fatalf("routed alias claim status=%q assignee=%q gc.routed_to=%q, want it reopened on its route",
				got.Status, got.Assignee, got.Metadata[beadmeta.RoutedToMetadataKey])
		}
	})
	t.Run("a routed alias claim a detached runtime may run stays on the alias", func(t *testing.T) {
		resetDetachedProbeHoldLogForTest()
		t.Cleanup(resetDetachedProbeHoldLogForTest)
		c := seat(t)
		spec := "tmux:" + filepath.Join(t.TempDir(), "gone") + ":soak-loop"
		held := createWork(t, c.store, "in_progress", "rig/claude", map[string]string{beadmeta.RoutedToMetadataKey: "rig/claude", detachedProbeMetadataKey: spec})
		closes(t, c)
		if got, _ := c.store.Get(held.ID); got.Assignee != "rig/claude" || got.Status != "in_progress" || got.Metadata[detachedProbeMetadataKey] != spec {
			t.Fatalf("detached claim assignee=%q status=%q gc.detached=%q, want it held on rig/claude with its probe",
				got.Assignee, got.Status, got.Metadata[detachedProbeMetadataKey])
		}
	})
	t.Run("a claim the row owns still refuses the close", func(t *testing.T) {
		c := seat(t)
		kept := createWork(t, c.store, "open", "rig/claude", nil)
		own := c.work(t, "in_progress", nil)
		if s := c.run(t, gone().tmux()); s.Outcome != settledRefused || s.Cause != causeHasWork {
			t.Fatalf("settlement %+v, want refused has-work", s)
		}
		if b := c.row(t); b.Status == "closed" {
			t.Fatal("row closed over a claim under its own bead ID")
		}
		if got, _ := c.store.Get(own.ID); got.Assignee != c.k.ID || got.Status != "in_progress" {
			t.Fatalf("row-owned claim assignee=%q status=%q, want kept", got.Assignee, got.Status)
		}
		if got, _ := c.store.Get(kept.ID); got.Assignee != "rig/claude" {
			t.Fatalf("alias claim assignee = %q, want kept while the row stays open", got.Assignee)
		}
	})
	t.Run("an agent's suspend closes it as an orphan", func(t *testing.T) {
		c := seat(t)
		c.entry().DrainReason = drainSuspended
		claim := createWork(t, c.store, "open", "rig/claude", nil)
		closes(t, c)
		givenBack(t, c, claim.ID)
	})
	t.Run("a city suspend keeps it", func(t *testing.T) {
		c := seat(t)
		c.entry().DrainReason = drainSuspended
		c.w.CitySuspended = true
		claim := createWork(t, c.store, "open", "rig/claude", nil)
		if it := c.decide(); it.Closing.Identity != "" {
			t.Fatalf("decideRow closing %+v under a city suspend, want no canonical singleton close", it.Closing)
		}
		if s := c.run(t, gone().tmux()); s.Closed {
			t.Fatalf("settlement %+v, want the row kept under a city suspend", s)
		}
		if got, _ := c.store.Get(claim.ID); got.Assignee != "rig/claude" {
			t.Fatalf("alias claim assignee = %q, want kept under a city suspend", got.Assignee)
		}
	})
}
