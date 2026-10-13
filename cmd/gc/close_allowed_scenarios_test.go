package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The R9-C review's table: a wake or restart request on an undesired or
// orphaned row, which nothing consumes, never keeps the row open. Both
// modes close it, as before closeAllowed.
func TestCloseAllowed_StaleRequestsCloseUndesiredRowsInBothModes(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	gone := func() *config.City {
		return &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "other", StartCommand: "true", MaxActiveSessions: intPtr(3)}}}
	}
	for _, f := range []parityFixture{
		{Name: "an undesired idle pool row, a stale wake", Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), "wake_request", "explicit", "wake_requested_at", at(-2*time.Hour))}},
		{Name: "an undesired idle pool row, a fresh wake", Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), "wake_request", "explicit", "wake_requested_at", at(-time.Minute))}},
		{Name: "a drained pool row, a stale wake", Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "drained", "slept_at", at(-time.Hour), "wake_request", "explicit", "wake_requested_at", at(-2*time.Hour))}},
		{Name: "a removed agent's pool row, a wake request", City: gone, Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), "wake_request", "explicit", "wake_requested_at", at(-time.Minute))}},
		{Name: "an undesired idle pool row, a restart request", Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_reason", "idle", "slept_at", at(-time.Hour), "restart_requested", "true")}},
	} {
		t.Run(f.Name, func(t *testing.T) {
			cityPath := t.TempDir()
			rows := f.converged(t, cityPath)
			l, v := legacyParity(t, f, cityPath, rows), v2Parity(t, f, cityPath, rows)
			if lr, vr := l.rows["gc-1"], v.rows["gc-1"]; lr.Status != "closed" || vr.Status != "closed" {
				t.Fatalf("legacy %s, v2 %s; want both closed", lr.Status, vr.Status)
			}
		})
	}
}

// The same rows through the legacy tick: an idle-timeout seat with a stale
// wake, and an orphan of a removed agent carrying a nudge's wake, close.
func TestCloseAllowed_StaleRequestsCloseInTheLegacyTick(t *testing.T) {
	t.Run("an idle-timeout seat, a stale wake", func(t *testing.T) {
		agent := persistentWorker()
		agent.MinActiveSessions = intPtr(0)
		e := r9cSeat(t, agent, nil)
		if err := e.mem.SetMetadataBatch(e.seat.ID, map[string]string{"sleep_reason": "idle-timeout", "wake_request": "explicit", "wake_requested_at": e.now.Add(-2 * time.Hour).Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			e.tick(t, nil)
			e.clk.Time = e.clk.Time.Add(time.Minute)
		}
		if b := e.reload(t, e.seat.ID); b.Status != "closed" {
			t.Fatalf("status %s, want closed", b.Status)
		}
	})
	t.Run("a removed agent's seat, a nudge's wake", func(t *testing.T) {
		e := r9cSeat(t, persistentWorker(), map[string]string{"sleep_reason": "idle", "wake_request": "explicit", "wake_requested_at": time.Date(2026, 10, 8, 11, 59, 0, 0, time.UTC).Format(time.RFC3339)})
		e.cfg.Agents = []config.Agent{otherLane()}
		for i := 0; i < 5; i++ {
			e.tick(t, nil)
			e.clk.Time = e.clk.Time.Add(10 * time.Minute)
		}
		if b := e.reload(t, e.seat.ID); b.Status != "closed" {
			t.Fatalf("status %s, want closed", b.Status)
		}
	})
}

// r9cSeat turns the killed-seat env's seat into the row under test.
func r9cSeat(t *testing.T, agent config.Agent, meta map[string]string) *killedSeatEnv {
	t.Helper()
	e := newKilledSeatEnv(t, agent)
	base := map[string]string{"sleep_reason": "", "slept_at": e.now.Add(-30 * time.Minute).Format(time.RFC3339)}
	for k, v := range meta {
		base[k] = v
	}
	if err := e.mem.SetMetadataBatch(e.seat.ID, base); err != nil {
		t.Fatal(err)
	}
	return e
}

// The drain-deadline retire of a stuck drain whose row carries a residual
// wake stops the runtime and closes the row: it never kills and then refuses.
func TestCloseAllowed_DrainDeadlineRetireClosesOverAWakeResidue(t *testing.T) {
	for _, state := range []string{"drained", "awake"} {
		t.Run(state, func(t *testing.T) {
			env := poolSeatEnv(t)
			env.rec = events.NewFake()
			seat := stuckDrainedPoolSeat(t, env, state, poolSlotDrainRetireDeadline+time.Minute)
			env.setSessionMetadata(&seat, map[string]string{"wake_request": "explicit", "wake_requested_at": env.clk.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)})
			for i := 0; i < 3; i++ {
				env.reconcile([]beads.Bead{seat})
				seat, _ = env.store.Get(seat.ID)
				env.clk.Time = env.clk.Time.Add(time.Minute)
			}
			if got, _ := env.store.Get(seat.ID); got.Status != "closed" || env.sp.IsRunning(poolSeatName) {
				t.Fatalf("status %s, running %v; want closed and stopped", got.Status, env.sp.IsRunning(poolSeatName))
			}
		})
	}
}

// R05: the tick's read lists live, the close-time re-read reads a
// controller's work cache. A claim the cache has not seen yet (written by
// another process, its event not applied) is visible to the tick's read
// alone, which keeps the seat.
func TestCloseRead_TheTickReadSeesWhatTheCacheMissed(t *testing.T) {
	c := newCloseReadCity(t)
	c.sw = testSeatWork(c.path, c.cfg, c.sessions, c.rigs)
	createWork(t, c.backing.Store, "in_progress", c.seat.ID, map[string]string{"gc.routed_to": "repo/worker"})
	if closeSeat(c.sessions, c.sw, c.seat, closeWhy{reason: "drained", gate: closeGated}, seatWorkNow, io.Discard) {
		t.Fatal("the gated close landed over a claim only the tick's live read saw")
	}
}

// R26: a duplicate shares the canonical's session_name, under which the
// canonical holds a claim. The duplicate's close is work-gated: it never
// releases the canonical's claim.
func TestCloseAllowed_DuplicateCloseSparesTheCanonicalsClaim(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)}
	template := "pack/worker"
	owner, err := store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:" + template},
		Metadata: map[string]string{"template": template, "agent_name": template, "state": "creating", "session_origin": "ephemeral", poolManagedMetadataKey: boolMetadata(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	sn := PoolSessionName(template, owner.ID)
	if err := store.SetMetadata(owner.ID, "session_name", sn); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title: "worker-2", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:" + template + "-2"},
		Metadata: map[string]string{"template": template, "session_name": sn, "agent_name": template + "-2", "pool_slot": "2", "state": "active", "session_origin": "ephemeral", poolManagedMetadataKey: boolMetadata(true)},
	}); err != nil {
		t.Fatal(err)
	}
	claim := createWork(t, store, "in_progress", sn, map[string]string{"gc.routed_to": template})
	ds := map[string]TemplateParams{sn: {TemplateName: template, InstanceName: template + "-2", PoolSlot: 2, Command: "codex"}}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{{Name: "worker", Dir: "pack", StartCommand: "true", MaxActiveSessions: intPtr(3)}}}
	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, runtime.NewFake(), allConfiguredDS(ds), cfg, clk, &stderr, false)
	if c, _ := store.Get(claim.ID); c.Assignee != sn {
		t.Fatalf("the canonical's claim is assigned to %q, want %q: the duplicate's close released it", c.Assignee, sn)
	}
}

// R25: the reconciler's close of an undesired failed create is work-gated:
// work assigned to it keeps it open.
func TestReconcileSessionBeads_FailedCreateCloseKeepsItsWork(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "polecat", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)}}}
	session := env.createSessionBead("polecat", "polecat-ga-mg0")
	env.setSessionMetadata(&session, map[string]string{"state": "failed-create", "pool_managed": "true", "pool_slot": "1"})
	createWork(t, env.store, "open", session.ID, nil)
	env.reconcile([]beads.Bead{session})
	if b, _ := env.store.Get(session.ID); b.Status == "closed" {
		t.Fatal("the failed create closed over its work")
	}
}

// The reconciler's not-running undesired close checks KeepsSlot on a pool
// row alone: a non-pool (manual) row an operator suspended, whose agent is gone,
// closes as legacy's did.
func TestReconcileSessionBeads_NonPoolUndesiredCloseIgnoresTheHold(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other", MaxActiveSessions: intPtr(1)}}}
	session := env.createSessionBead("gone-1", "gone")
	env.setSessionMetadata(&session, map[string]string{"session_origin": "manual", "state": "asleep", "sleep_reason": "user-hold", "sleep_intent": "user-hold", "held_until": env.clk.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)})
	env.reconcile([]beads.Bead{session})
	if b, _ := env.store.Get(session.ID); b.Status != "closed" {
		t.Fatalf("status %s, want the non-pool orphan closed whatever holds it", b.Status)
	}
}

// An undesired pool row under an operator's suspend that holds a claim,
// served or not: its free-slot close refuses keeps-slot, and the orphan
// follow-up neither releases the claim nor closes the row, in both modes.
func TestCloseAllowed_AHeldUndesiredRowKeepsItsClaims(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	for _, tc := range []struct{ name, route string }{{"a served claim", "worker"}, {"an unserved claim", "nowhere/lane"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := parityFixture{
				Name: tc.name,
				Rows: []parityRow{poolRow("gc-1", "worker", 1, "asleep", "sleep_intent", "user-hold", "sleep_reason", "user-hold", "held_until", at(100*24*time.Hour), "slept_at", at(-time.Hour))},
				Work: []parityRow{{ID: "gw-1", Title: "gw-1", Type: "task", Status: "in_progress", Assignee: "gc-1", Metadata: map[string]string{"gc.routed_to": tc.route}}},
			}
			cityPath := t.TempDir()
			rows := f.converged(t, cityPath)
			for mode, out := range map[string]parityOutcome{"legacy": legacyParity(t, f, cityPath, rows), "v2": v2Parity(t, f, cityPath, rows)} {
				if r, w := out.rows["gc-1"], out.work["gw-1"]; r.Status == "closed" || w.Assignee != "gc-1" {
					t.Errorf("%s: row %s, claim assigned to %q; want the held row open with its claim", mode, r.Status, w.Assignee)
				}
			}
		})
	}
}

// The same through the legacy tick: a held undesired seat keeps its served
// claim on another lane and its unserved one, and stays open.
func TestCloseAllowed_AHeldSeatKeepsItsClaimsInTheLegacyTick(t *testing.T) {
	for _, tc := range []struct{ name, route string }{{"a served claim", "repo/other"}, {"an unserved claim", "nowhere/lane"}} {
		t.Run(tc.name, func(t *testing.T) {
			agent := persistentWorker()
			agent.MinActiveSessions = intPtr(0)
			e := r9cSeat(t, agent, map[string]string{"state": "suspended", "sleep_reason": "user-hold", "sleep_intent": "user-hold", "held_until": time.Date(2126, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339), "suspended_at": time.Date(2026, 10, 8, 11, 0, 0, 0, time.UTC).Format(time.RFC3339)})
			claimID := e.addWork(t, "in_progress", e.seat.ID, map[string]string{"gc.routed_to": tc.route}).ID
			for i := 0; i < 3; i++ {
				e.tick(t, claims(e.reload(t, claimID)))
				e.clk.Time = e.clk.Time.Add(time.Minute)
			}
			if b, c := e.reload(t, e.seat.ID), e.reload(t, claimID); b.Status == "closed" || c.Assignee != e.seat.ID {
				t.Fatalf("seat %s, claim assigned to %q; want the held seat open with its claim", b.Status, c.Assignee)
			}
		})
	}
}
