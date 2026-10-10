package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Killed pool seats (owner ruling B1, CONTRACT v5.8a C3 "Killed pool seats",
// §12.2 row 28, SESS-623/625). A pool row `gc session kill` stopped, with no
// honored kill fence, frees its slot. A seat holding no started work gives back
// its open claims (with its fallback route) and closes; a seat holding started
// work releases nothing and is woken in place while the work has demand, or
// holds its slot asleep while it is blocked; it never takes the stranded branch.

// killRaceStore lets a test interleave a concurrent actor with the reconciler's
// reads. afterList runs after every List the wrapped store answers. Conditional
// writes resolve to the wrapped store, so releases keep their CAS.
type killRaceStore struct {
	beads.Store
	afterList func(q beads.ListQuery, got []beads.Bead)
	getErr    map[string]error
	listErr   map[string]error // by query status
}

func (s *killRaceStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if err := s.listErr[q.Status]; err != nil {
		return nil, err
	}
	got, err := s.Store.List(q)
	if err == nil && s.afterList != nil {
		s.afterList(q, got)
	}
	return got, err
}

func (s *killRaceStore) Get(id string) (beads.Bead, error) {
	if err := s.getErr[id]; err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

func (s *killRaceStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// killedSeatEnv is a pool whose seat, slot 1, was stopped by `gc session
// kill`: asleep, sleep_reason=killed, the kill fence already lifted, runtime
// gone.
type killedSeatEnv struct {
	now   time.Time
	city  string
	cfg   *config.City
	mem   beads.Store
	store *killRaceStore
	sp    *runtime.Fake
	clk   *clock.Fake
	dt    *drainTracker
	seat  beads.Bead
	dops  drainOps // nil: a fresh fake per tick
}

// persistentWorker is a persistent pool with a floor of one seat.
func persistentWorker() config.Agent {
	return config.Agent{Name: "worker", Dir: "repo", StartCommand: "true", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2)}
}

func newKilledSeatEnv(t *testing.T, agent config.Agent) *killedSeatEnv {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "kill-town", agent.Name)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "kill-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents:    []config.Agent{agent, otherLane()},
	}
	mem := beads.NewMemStore()
	seat := createCanonicalPoolSession(t, mem, &cfg.Agents[0], now.Add(-time.Hour), 1)
	if err := mem.SetMetadataBatch(seat.ID, map[string]string{
		"state":                     "asleep",
		"sleep_reason":              string(session.SleepReasonKilled),
		"slept_at":                  now.Add(-time.Minute).Format(time.RFC3339),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark seat killed: %v", err)
	}
	seat, err := mem.Get(seat.ID)
	if err != nil {
		t.Fatalf("reload seat: %v", err)
	}
	return &killedSeatEnv{
		now:   now,
		city:  cityDir,
		cfg:   cfg,
		mem:   mem,
		store: &killRaceStore{Store: mem},
		sp:    runtime.NewFake(),
		clk:   &clock.Fake{Time: now},
		dt:    newDrainTracker(),
		seat:  seat,
	}
}

// otherLane is a second configured pool, the lane routedClaim routes to.
func otherLane() config.Agent {
	return config.Agent{Name: "other", Dir: "repo", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}
}

// routedClaim is the metadata of a lane-visible claim: one the kill path
// releases (claimIsLaneVisible). It is routed to another configured lane, so
// it never becomes demand for the seat's own pool.
func routedClaim() map[string]string {
	return map[string]string{beadmeta.RoutedToMetadataKey: "repo/other"}
}

// claims is the tick's assigned-work snapshot of the given claims, as the
// controller's open-routed pass collects them: no readiness verdict, so no
// wake demand.
func claims(bs ...beads.Bead) []beads.Bead { return bs }

// addWork creates a work bead assigned to assignee with the given status and
// metadata.
func (e *killedSeatEnv) addWork(t *testing.T, status, assignee string, meta map[string]string) beads.Bead {
	t.Helper()
	work, err := e.mem.Create(beads.Bead{Title: "work " + status, Type: "task", Status: "open", Assignee: assignee, Metadata: meta})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if status != "open" {
		if err := e.mem.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("set work status %s: %v", status, err)
		}
	}
	return e.reload(t, work.ID)
}

// tick runs one legacy reconcile pass over the seat. assigned is the tick's
// actionable assigned-work snapshot, the input AL1's assigned-work pass reads.
func (e *killedSeatEnv) tick(t *testing.T, assigned []beads.Bead, opts ...startExecutionOption) {
	t.Helper()
	e.tickWith(t, e.reload(t, e.seat.ID), assigned, opts...)
}

// tickWith runs the pass over a given snapshot of the seat.
func (e *killedSeatEnv) tickWith(t *testing.T, seat beads.Bead, assigned []beads.Bead, opts ...startExecutionOption) {
	t.Helper()
	ds := buildDesiredState("kill-town", e.city, e.now, e.cfg, e.sp, e.mem, io.Discard)
	var dops drainOps = newFakeDrainOps()
	if e.dops != nil {
		dops = e.dops
	}
	reconcileSessionBeads(
		context.Background(), []beads.Bead{seat}, ds.State, map[string]bool{e.cfg.Agents[0].QualifiedName(): true},
		e.cfg, e.sp, e.store, dops, assigned, nil, e.dt, ds.PoolDesiredCounts, false, nil, "kill-town",
		nil, e.clk, events.Discard, 0, 0, io.Discard, io.Discard, opts...,
	)
}

func (e *killedSeatEnv) reload(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := e.mem.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// openWorkerSeats lists the open session beads.
func (e *killedSeatEnv) openWorkerSeats(t *testing.T) []beads.Bead {
	t.Helper()
	all, err := e.mem.List(beads.ListQuery{Type: sessionBeadType})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var open []beads.Bead
	for _, b := range all {
		if b.Status != "closed" {
			open = append(open, b)
		}
	}
	return open
}

func (e *killedSeatEnv) assertAssigned(t *testing.T, work beads.Bead, status, assignee string) {
	t.Helper()
	got := e.reload(t, work.ID)
	if got.Status != status || got.Assignee != assignee {
		t.Fatalf("work %s: status=%q assignee=%q, want %q/%q", work.ID, got.Status, got.Assignee, status, assignee)
	}
}

// assertClosedAsKilled checks the pool-slot close's ClosePatch(killed).
func assertClosedAsKilled(t *testing.T, seat beads.Bead) {
	t.Helper()
	if seat.Status != "closed" {
		t.Fatalf("killed seat status = %q (state=%q sleep_reason=%q), want closed", seat.Status, seat.Metadata["state"], seat.Metadata["sleep_reason"])
	}
	if got, want := seat.Metadata["state"], string(session.SleepReasonKilled); got != want {
		t.Fatalf("closed seat state = %q, want %q", got, want)
	}
	if got, want := seat.Metadata["close_reason"], session.CanonicalCloseReason(string(session.SleepReasonKilled)); got != want {
		t.Fatalf("closed seat close_reason = %q, want %q", got, want)
	}
}

// assertKeptOpen checks that the seat is open and took no stranded branch.
func assertKeptOpen(t *testing.T, seat beads.Bead) {
	t.Helper()
	if seat.Status == "closed" {
		t.Fatalf("killed seat closed (close_reason=%q); want it kept", seat.Metadata["close_reason"])
	}
	if got := seat.Metadata[strandedEventEmittedKey]; got != "" {
		t.Fatalf("killed seat took the stranded branch: %s=%q", strandedEventEmittedKey, got)
	}
}

// J39: an idle killed persistent seat closes (ClosePatch(killed)) and the
// pool's floor creates a fresh seat with a fresh identity in its slot. Legacy
// kept the killed row open and asleep, holding the slot for good.
func TestReconcileSessionBeads_KilledIdlePoolSeatClosesAndIsReplaced(t *testing.T) {
	e := newKilledSeatEnv(t, persistentWorker())

	e.tick(t, nil)

	assertClosedAsKilled(t, e.reload(t, e.seat.ID))
	buildDesiredState("kill-town", e.city, e.now, e.cfg, e.sp, e.mem, io.Discard)
	open := e.openWorkerSeats(t)
	if len(open) != 1 || open[0].ID == e.seat.ID {
		t.Fatalf("open worker seats after close = %+v, want one fresh seat (not %s)", open, e.seat.ID)
	}
}

// A killed idle one_shot seat is closed by legacy in the same tick, like a
// persistent one: asleep rows get no pool demand, so it is not reused in place.
// (With no floor the row is not desired at all and the orphan close takes it.)
func TestReconcileSessionBeads_KilledIdleOneShotSeatClosesInTheSameTick(t *testing.T) {
	agent := persistentWorker()
	agent.Lifecycle = config.AgentLifecycleOneShot
	e := newKilledSeatEnv(t, agent)

	e.tick(t, nil)

	assertClosedAsKilled(t, e.reload(t, e.seat.ID))
}

// Rule 2 (v5.8d): a seat with no started work gives back its lane-visible
// claims before the close, with its fallback route (its template); a bead's
// own routing is kept. A claim is lane-visible when its route (gc.routed_to,
// or a workflow bead's gc.run_target, stamped with the fallback route when
// empty) resolves to a configured agent. The release reads the store live, so
// it also gives back the workflow bead the tick snapshot does not carry (it
// has no route of its own yet), and the released workflow bead is demanded.
// Kept, each stranded by a release: a directly assigned plain task, a bead
// routed to an unserved lane, and an expanded workflow root (whose run_target
// is no demand or claim route). The close finds them and refuses; the seat
// holds its slot with no stranded branch, and the assigned-work wake restarts
// it when that work is ready.
func TestReconcileSessionBeads_KilledPoolSeatReleasesOnlyLaneVisibleWork(t *testing.T) {
	agent := persistentWorker()
	agent.Dir = "" // a city-scoped pool, so the released bead's store is the probe's
	agent.MinActiveSessions = intPtr(0)
	e := newKilledSeatEnv(t, agent)
	routed := e.addWork(t, "open", e.seat.ID, routedClaim())
	workflow := e.addWork(t, "open", e.seat.ID, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow})
	plain := e.addWork(t, "open", e.seat.ID, nil)
	unserved := e.addWork(t, "open", e.seat.ID, map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"})
	expanded := e.addWork(t, "open", e.seat.ID, map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowExpandedMetadataKey: "true",
	})

	e.tick(t, claims(routed, unserved))

	if got := e.reload(t, routed.ID); got.Assignee != "" || got.Metadata[beadmeta.RunTargetMetadataKey] != "" {
		t.Fatalf("routed work: assignee=%q run_target=%q, want released and its own routing kept", got.Assignee, got.Metadata[beadmeta.RunTargetMetadataKey])
	}
	got := e.reload(t, workflow.ID)
	if got.Status != "open" || got.Assignee != "" || got.Metadata[beadmeta.RunTargetMetadataKey] != "worker" {
		t.Fatalf("workflow work: status=%q assignee=%q %s=%q, want open, unassigned, routed to the seat's template worker",
			got.Status, got.Assignee, beadmeta.RunTargetMetadataKey, got.Metadata[beadmeta.RunTargetMetadataKey])
	}
	for _, kept := range []beads.Bead{plain, unserved, expanded} {
		e.assertAssigned(t, kept, "open", e.seat.ID)
		if got := e.reload(t, kept.ID).Metadata[beadmeta.RunTargetMetadataKey]; got != "" {
			t.Fatalf("kept work %s was stamped %s=%q", kept.ID, beadmeta.RunTargetMetadataKey, got)
		}
	}
	seat := e.reload(t, e.seat.ID)
	assertKeptOpen(t, seat)
	if seat.Metadata["state"] != "asleep" || seat.Metadata["sleep_reason"] != string(session.SleepReasonKilled) {
		t.Fatalf("seat state=%q sleep_reason=%q, want asleep/killed holding its slot", seat.Metadata["state"], seat.Metadata["sleep_reason"])
	}

	ds := buildDesiredState("kill-town", e.city, e.now, e.cfg, e.sp, e.mem, io.Discard)
	if got := ds.ScaleCheckCounts["worker"]; got != 1 {
		t.Fatalf("worker demand = %d (%v), want 1: the released workflow bead", got, ds.ScaleCheckCounts)
	}
}

// The release runs only when the tick's snapshot shows a lane-visible open
// claim for the seat; otherwise the pass reads nothing more and the close's
// live work read decides. Here the only releasable claim is a workflow bead
// with no route of its own, which the controller's snapshot does not carry:
// nothing is released, and the seat holds its slot until the bead is ready.
func TestReconcileSessionBeads_KilledPoolSeatSkipsTheReleaseWithoutASnapshotClaim(t *testing.T) {
	e := newKilledSeatEnv(t, persistentWorker())
	workflow := e.addWork(t, "open", e.seat.ID, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow})
	var startedReads int
	e.store.afterList = func(q beads.ListQuery, _ []beads.Bead) {
		if q.Status == "in_progress" && q.Assignee == e.seat.ID && q.TierMode == beads.TierBoth {
			startedReads++
		}
	}

	e.tick(t, nil)

	e.assertAssigned(t, workflow, "open", e.seat.ID)
	assertKeptOpen(t, e.reload(t, e.seat.ID))
	if startedReads != 0 {
		t.Fatalf("started-work gate read the store %d times with no releasable claim in the snapshot", startedReads)
	}
}

// The release is guarded by poolFreeable, which the live re-read does not
// repeat in full: the re-read checks the row is open, killed and freeable by
// state, not that it is a pool seat. A killed manual session is not a pool
// seat, so its routed claim stays with it.
func TestReconcileSessionBeads_KilledManualSessionKeepsItsRoutedClaim(t *testing.T) {
	e := newKilledSeatEnv(t, persistentWorker())
	if err := e.mem.SetMetadataBatch(e.seat.ID, map[string]string{
		poolManagedMetadataKey: "", "pool_slot": "", "canonical_pool_slot": "", "session_origin": "manual", "manual_session": "true",
	}); err != nil {
		t.Fatal(err)
	}
	claim := e.addWork(t, "open", e.seat.ID, routedClaim())

	e.tick(t, claims(claim))

	e.assertAssigned(t, claim, "open", e.seat.ID)
}

// Rule 2's identities include the stable alias that namepool and
// max_active_sessions = 1 seats claim under, the set the close's work read
// uses. A narrower release would leave an alias claim assigned and the close
// refusing forever.
func TestReconcileSessionBeads_KilledPoolSeatReleasesAliasClaims(t *testing.T) {
	namepool := persistentWorker()
	namepool.NamepoolNames = []string{"ada", "bob"}
	singleton := persistentWorker()
	singleton.MaxActiveSessions = intPtr(1)
	for name, agent := range map[string]config.Agent{"namepool": namepool, "max_active_sessions=1": singleton} {
		t.Run(name, func(t *testing.T) {
			e := newKilledSeatEnv(t, agent)
			alias := e.seat.Metadata["alias"]
			if alias == "" || alias == e.seat.ID || alias == e.seat.Metadata["session_name"] {
				t.Fatalf("fixture seat has no distinct stable alias: %v", e.seat.Metadata)
			}
			// The routed claim under the seat's ID puts the seat's release in
			// the snapshot; the alias claim, a workflow bead with no route of
			// its own, is found only by the release's live sweep over every
			// identity.
			routed := e.addWork(t, "open", e.seat.ID, routedClaim())
			claim := e.addWork(t, "open", alias, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow})

			e.tick(t, claims(routed))

			e.assertAssigned(t, routed, "open", "")
			e.assertAssigned(t, claim, "open", "")
			assertClosedAsKilled(t, e.reload(t, e.seat.ID))
		})
	}
}

// Rule 2 is gated on rule 1: a seat holding started work releases nothing, so
// it keeps its whole context, including pre-assigned molecule siblings and
// their gc.continuation_group. Started is in_progress, or open work the row
// names as currently_processing_bead_id or current_claim_bead_id. The started
// work here is blocked or not ready, so there is no wake demand and the close
// runs, refuses, and the seat holds its slot asleep. It never takes the
// stranded branch, even past the stranded repair's confirmation window.
func TestReconcileSessionBeads_KilledPoolSeatWithStartedWorkReleasesNothing(t *testing.T) {
	evidence := func(key string) func(e *killedSeatEnv, t *testing.T) beads.Bead {
		return func(e *killedSeatEnv, t *testing.T) beads.Bead {
			w := e.addWork(t, "open", e.seat.ID, nil)
			if err := e.mem.SetMetadata(e.seat.ID, key, w.ID); err != nil {
				t.Fatal(err)
			}
			return w
		}
	}
	cases := map[string]func(e *killedSeatEnv, t *testing.T) beads.Bead{
		"blocked in_progress": func(e *killedSeatEnv, t *testing.T) beads.Bead {
			return e.addWork(t, "in_progress", e.seat.ID, nil)
		},
		session.CurrentBeadIDKey:               evidence(session.CurrentBeadIDKey),
		beadmeta.CurrentClaimBeadIDMetadataKey: evidence(beadmeta.CurrentClaimBeadIDMetadataKey),
	}
	blocked := true
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			e := newKilledSeatEnv(t, persistentWorker())
			started := seed(e, t)
			sibling := e.addWork(t, "open", e.seat.ID, map[string]string{beadmeta.ContinuationGroupMetadataKey: "mol-1", beadmeta.RoutedToMetadataKey: "repo/other"})
			// Blocked in_progress work is in the actionable snapshot with no
			// wake demand; open work with execution evidence is not ready. The
			// routed sibling is in the snapshot, so the pass reaches the gate.
			assigned := claims(sibling)
			if started.Status == "in_progress" {
				snap := started
				snap.IsBlocked = &blocked
				assigned = append(assigned, snap)
			}

			e.tick(t, assigned)
			e.clk.Time = e.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
			e.now = e.clk.Time
			e.tick(t, assigned)

			got := e.reload(t, e.seat.ID)
			assertKeptOpen(t, got)
			if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != string(session.SleepReasonKilled) || e.sp.IsRunning(got.Metadata["session_name"]) {
				t.Fatalf("seat state=%q sleep_reason=%q running=%v, want asleep/killed holding its slot", got.Metadata["state"], got.Metadata["sleep_reason"], e.sp.IsRunning(got.Metadata["session_name"]))
			}
			e.assertAssigned(t, started, started.Status, e.seat.ID)
			e.assertAssigned(t, sibling, "open", e.seat.ID)
			if g := e.reload(t, sibling.ID).Metadata[beadmeta.ContinuationGroupMetadataKey]; g != "mol-1" {
				t.Fatalf("sibling continuation group = %q, want mol-1 kept", g)
			}
		})
	}
}

// Rule 1: a killed seat holding unblocked in_progress work, or ready open
// work, is woken in place on its bead by AL1's assigned-work pass; nothing is
// released and the row is not closed.
func TestReconcileSessionBeads_KilledPoolSeatWithDemandWakesInPlace(t *testing.T) {
	for _, status := range []string{"in_progress", "open"} {
		t.Run(status, func(t *testing.T) {
			e := newKilledSeatEnv(t, persistentWorker())
			work := e.addWork(t, status, e.seat.ID, nil)
			unready := e.addWork(t, "open", e.seat.ID, routedClaim())

			// Only the first bead is in the actionable snapshot, and it is ready.
			e.tick(t, []beads.Bead{work}, withReadyAssignedFlags([]bool{true}))

			got := e.reload(t, e.seat.ID)
			assertKeptOpen(t, got)
			if !e.sp.IsRunning(got.Metadata["session_name"]) {
				t.Fatalf("killed seat with %s demand not woken: state=%q sleep_reason=%q", status, got.Metadata["state"], got.Metadata["sleep_reason"])
			}
			e.assertAssigned(t, work, status, e.seat.ID)
			e.assertAssigned(t, unready, "open", e.seat.ID)
		})
	}
}

// An honored kill fence in the tick's snapshot means the kill still owns the
// row: not freeable, so neither the release nor the close runs.
func TestReconcileSessionBeads_KilledPoolSeatWithPendingKillFenceIsNotFreed(t *testing.T) {
	e := newKilledSeatEnv(t, persistentWorker())
	if err := e.mem.SetMetadata(e.seat.ID, "state_reason", session.KillPendingReason); err != nil {
		t.Fatalf("stamp kill fence: %v", err)
	}
	unstarted := e.addWork(t, "open", e.seat.ID, routedClaim())

	e.tick(t, claims(unstarted))

	assertKeptOpen(t, e.reload(t, e.seat.ID))
	e.assertAssigned(t, unstarted, "open", e.seat.ID)
}

// The release re-reads the row live and acts only while it is still an open,
// freeable, killed row. Each case changes the live row after the tick's
// snapshot was taken (the snapshot itself is a fence-free killed row, so the
// pass reaches the release) and fails a different one of those checks. The
// last case pins that the started-work read fails closed.
func TestReleaseUnexecutedClaimsOnKill_RechecksTheLiveRow(t *testing.T) {
	cases := map[string]func(e *killedSeatEnv, t *testing.T){
		"closed": func(e *killedSeatEnv, t *testing.T) {
			if err := e.mem.Close(e.seat.ID); err != nil {
				t.Fatal(err)
			}
		},
		"re-slept with another reason": func(e *killedSeatEnv, t *testing.T) {
			if err := e.mem.SetMetadata(e.seat.ID, "sleep_reason", "idle"); err != nil {
				t.Fatal(err)
			}
		},
		"woken": func(e *killedSeatEnv, t *testing.T) {
			if err := e.mem.SetMetadata(e.seat.ID, "state", "awake"); err != nil {
				t.Fatal(err)
			}
		},
		"re-killed (fence pending)": func(e *killedSeatEnv, t *testing.T) {
			if err := e.mem.SetMetadataBatch(e.seat.ID, map[string]string{
				"state_reason": session.KillPendingReason,
				"slept_at":     e.now.Format(time.RFC3339),
			}); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(e *killedSeatEnv, _ *testing.T) {
			e.store.getErr = map[string]error{e.seat.ID: errors.New("store unavailable")}
		},
		// Not a row change: the started-work read fails, and fails closed.
		"started-work read fails": func(e *killedSeatEnv, _ *testing.T) {
			e.store.listErr = map[string]error{"in_progress": errors.New("leg is dark")}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := newKilledSeatEnv(t, persistentWorker())
			unstarted := e.addWork(t, "open", e.seat.ID, routedClaim())
			snapshot := sessionInfosFromBeads([]beads.Bead{e.seat})[0]
			change(e, t)

			releaseUnexecutedClaimsOnKill("", e.cfg, e.store, nil, e.now, snapshot, time.Minute, io.Discard)

			e.assertAssigned(t, unstarted, "open", e.seat.ID)
		})
	}
}

// The release reads every leg of the assigned-work sweep: a claim the seat
// holds in a rig store is released, and started work there gates the release
// everywhere.
func TestReleaseUnexecutedClaimsOnKill_ReadsRigLegs(t *testing.T) {
	for _, startedInRig := range []bool{false, true} {
		name := "unstarted"
		if startedInRig {
			name = "started in rig"
		}
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			seedNoRoutes(t, cityPath)
			cfg := &config.City{
				Workspace: config.Workspace{Name: "kill-town"},
				Rigs:      []config.Rig{{Name: "riga", Path: filepath.Join(cityPath, "riga")}},
				Agents:    []config.Agent{{Name: "worker", Dir: "riga"}},
			}
			cityStore, rigStore := beads.NewMemStore(), beads.NewMemStore()
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			seat, err := cityStore.Create(beads.Bead{Type: sessionBeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
				"template": "riga/worker", "session_name": "kill-town--worker-1", "pool_managed": "true", "pool_slot": "1",
				"state": "asleep", "sleep_reason": string(session.SleepReasonKilled), "slept_at": now.Add(-time.Minute).Format(time.RFC3339),
			}})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := rigStore.Create(beads.Bead{Title: "rig claim", Type: "task", Assignee: seat.ID, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"}})
			if err != nil {
				t.Fatal(err)
			}
			if startedInRig {
				inProgress := "in_progress"
				started, err := rigStore.Create(beads.Bead{Title: "rig started", Type: "task", Assignee: seat.ID})
				if err != nil {
					t.Fatal(err)
				}
				if err := rigStore.Update(started.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
					t.Fatal(err)
				}
			}

			releaseUnexecutedClaimsOnKill(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, now,
				sessionInfosFromBeads([]beads.Bead{seat})[0], time.Minute, io.Discard)

			got, err := rigStore.Get(claim.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if startedInRig {
				want = seat.ID
			}
			if got.Assignee != want {
				t.Fatalf("rig claim assignee = %q, want %q", got.Assignee, want)
			}
		})
	}
}

// Concurrency: work assigned to the seat after its release and before the
// close's live work read is found by that read: the close refuses, with no
// stranded branch. The next pass releases it (it is not ready) and closes.
func TestReconcileSessionBeads_KilledPoolSeatWorkAssignedAfterReleaseRefusesTheClose(t *testing.T) {
	e := newKilledSeatEnv(t, persistentWorker())
	unstarted := e.addWork(t, "open", e.seat.ID, routedClaim())
	var late beads.Bead
	e.store.afterList = func(beads.ListQuery, []beads.Bead) {
		if late.ID == "" && e.reload(t, unstarted.ID).Assignee == "" {
			late = e.addWork(t, "open", e.seat.ID, routedClaim())
		}
	}

	e.tick(t, claims(unstarted))

	if late.ID == "" {
		t.Fatal("fixture never assigned the late work")
	}
	assertKeptOpen(t, e.reload(t, e.seat.ID))
	e.assertAssigned(t, late, "open", e.seat.ID)

	e.store.afterList = nil
	e.tick(t, claims(e.reload(t, late.ID)))
	e.assertAssigned(t, late, "open", "")
	assertClosedAsKilled(t, e.reload(t, e.seat.ID))
}

// Concurrency: a claim that changes hands between the release's list and its
// write is left with its new holder (ReleaseWorkBead's assignee CAS); the
// seat then holds nothing and closes.
func TestReconcileSessionBeads_KilledPoolSeatLeavesAClaimTakenMidRelease(t *testing.T) {
	e := newKilledSeatEnv(t, persistentWorker())
	claim := e.addWork(t, "open", e.seat.ID, routedClaim())
	other := "other-seat"
	e.store.afterList = func(q beads.ListQuery, got []beads.Bead) {
		if q.Status == "open" && q.Assignee == e.seat.ID && len(got) > 0 {
			if err := e.mem.Update(claim.ID, beads.UpdateOpts{Assignee: &other}); err != nil {
				t.Fatal(err)
			}
		}
	}

	e.tick(t, claims(claim))

	e.assertAssigned(t, claim, "open", other)
	assertClosedAsKilled(t, e.reload(t, e.seat.ID))
}

// Concurrency: a wake, attach or new kill fence that lands after the release
// listed the seat's open claims does not stop those listed claims from being
// released. That is the accepted window: the live re-read is a check, not a
// fence, and only open work with no execution evidence is exposed (a seat
// holding started work releases nothing). The close itself is fenced on the
// row the pass decided on, so it does not land over the change.
func TestReconcileSessionBeads_KilledPoolSeatRowChangeMidReleaseKeepsTheSeat(t *testing.T) {
	cases := map[string]map[string]string{
		"wake":       {"state": "awake", "sleep_reason": ""},
		"attach":     {"state": "active", "sleep_reason": ""},
		"kill fence": {"state_reason": session.KillPendingReason, "slept_at": "2026-10-08T12:00:00Z"},
	}
	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			e := newKilledSeatEnv(t, persistentWorker())
			claim := e.addWork(t, "open", e.seat.ID, routedClaim())
			done := false
			e.store.afterList = func(q beads.ListQuery, got []beads.Bead) {
				if !done && q.Status == "open" && q.Assignee == e.seat.ID && len(got) > 0 {
					done = true
					if err := e.mem.SetMetadataBatch(e.seat.ID, patch); err != nil {
						t.Fatal(err)
					}
				}
			}

			e.tick(t, claims(claim))

			e.assertAssigned(t, claim, "open", "")
			if got := e.reload(t, e.seat.ID); got.Status == "closed" {
				t.Fatalf("close landed over a %s that arrived mid-release", name)
			}
		})
	}
}

// A one_shot pool's reuse predicate admits an idle killed seat (the freeable
// allowlist is shared); a fenced or work-holding killed seat is not reusable,
// and a persistent pool never reuses an asleep row. The predicate reads the
// build's clock, not the wall clock.
func TestReusablePoolSessionInfo_OneShotKilledIdleSeat(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	bp := &agentBuildParams{decisionTime: now}
	oneShot := &config.Agent{Name: "worker", Dir: "repo", Lifecycle: config.AgentLifecycleOneShot}
	template := "repo/worker"
	killed := session.Info{
		ID:                  "session-killed",
		Template:            template,
		SessionNameMetadata: "repo--worker-1-pool",
		PoolManaged:         true,
		PoolSlot:            "1",
		MetadataState:       "asleep",
		SleepReason:         string(session.SleepReasonKilled),
		SleptAt:             now.Add(-time.Minute).Format(time.RFC3339),
	}
	if !reusablePoolSessionInfo(bp, oneShot, template, killed, nil) {
		t.Fatal("one_shot idle killed seat not admitted by the reuse predicate")
	}

	fenced := killed
	fenced.StateReason = session.KillPendingReason
	if reusablePoolSessionInfo(bp, oneShot, template, fenced, nil) {
		t.Fatal("one_shot killed seat under an honored kill fence reused")
	}
	// The same fence read at a build time past the grace is not honored.
	late := &agentBuildParams{decisionTime: now.Add(session.KillPendingGrace + time.Minute)}
	if !reusablePoolSessionInfo(late, oneShot, template, fenced, nil) {
		t.Fatal("reuse predicate did not read the build clock: a stale fence is not honored")
	}

	withWork := &agentBuildParams{decisionTime: now, assignedWorkBeads: []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: killed.ID}}}
	if reusablePoolSessionInfo(withWork, oneShot, template, killed, nil) {
		t.Fatal("one_shot killed seat holding work reused; rules 1-3 must apply to it instead")
	}

	persistent := &config.Agent{Name: "worker", Dir: "repo"}
	if reusablePoolSessionInfo(bp, persistent, template, killed, nil) {
		t.Fatal("persistent pool reused an asleep killed row")
	}
}

// D5's drain-ack release is unchanged by the shared body: its budget line
// still names the dead-assignee sweep.
func TestDrainAckReleaseBudgetLogTextIsUnchanged(t *testing.T) {
	work := beads.NewMemStore()
	var stderr bytes.Buffer

	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), -time.Second, &stderr)

	want := "session beads: held-claim release for draining session sess-1 ran out of its -1s budget; remaining legs are left to the dead-assignee sweep\n"
	if stderr.String() != want {
		t.Fatalf("drain-ack budget log = %q, want %q", stderr.String(), want)
	}
}

// D5's drain-ack release still passes no fallback route: a released bead keeps
// whatever routing it carried (none here), unlike the killed seat's release.
func TestDrainAckReleaseStampsNoFallbackRoute(t *testing.T) {
	work := beads.NewMemStore()
	held := mustCreateDrainAckBead(t, work, beads.Bead{Title: "unrouted claim", Type: "task"}, "in_progress", "worker-1")

	releaseUnexecutedClaimsOnDrainAck("", nil, work, nil, drainAckSessionBead(), time.Minute, io.Discard)

	got, err := work.Get(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.Metadata[beadmeta.RunTargetMetadataKey] != "" {
		t.Fatalf("drain-ack release: assignee=%q %s=%q, want released with no route stamped", got.Assignee, beadmeta.RunTargetMetadataKey, got.Metadata[beadmeta.RunTargetMetadataKey])
	}
}

// The legacy build's pool decisions read the build's own clock, not its
// beaconTime, which `gc start` fixes for the controller's lifetime. With a
// day-old beacon, a kill fence stamped now is still honored, so the fenced
// one_shot row is not reused and a fresh seat is created. Read off the day-old
// beacon, the fence would look a day in the future, go unhonored, and the
// fenced row would be reused.
func TestBuildDesiredState_PoolDecisionsReadTheBuildClock(t *testing.T) {
	agent := persistentWorker()
	agent.Lifecycle = config.AgentLifecycleOneShot
	e := newKilledSeatEnv(t, agent)
	now := time.Now().UTC()
	if err := e.mem.SetMetadataBatch(e.seat.ID, map[string]string{
		"state_reason": session.KillPendingReason,
		"slept_at":     now.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	buildDesiredState("kill-town", e.city, now.Add(-24*time.Hour), e.cfg, e.sp, e.mem, io.Discard)

	if got := len(e.openWorkerSeats(t)); got != 2 {
		t.Fatalf("open seats = %d, want 2: the fenced killed row is not reused, a fresh seat is created", got)
	}
}

// The post-tick desired-state refresh reads its own clock too. Its dependency
// floor defers a singleton alias normalization that collides, and stamps the
// retry time: now, not the refresh's day-old beacon.
func TestRefreshDesiredState_PoolDecisionsReadTheRefreshClock(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	mk := func(meta map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Title: meta["session_name"], Type: sessionBeadType, Labels: []string{sessionBeadLabel, "template:" + meta["template"]}, Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	app := mk(map[string]string{"template": "app", "agent_name": "app", "session_name": "s-app", "state": "awake"})
	dep := mk(map[string]string{
		"template": "cashmaster/refinery", "agent_name": "cashmaster/refinery-1", "alias": "cashmaster/refinery-1", "session_name": "s-dep",
		"state": "awake", "dependency_only": "true", poolManagedMetadataKey: "true", "pool_slot": "1", poolAliasConflictCountMetadataKey: "2",
	})
	// Holds the canonical alias the dependency would normalize to.
	blocker := mk(map[string]string{"template": "app", "agent_name": "app-2", "alias": "cashmaster/refinery", "session_name": "s-blocker", "state": "awake"})
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{
		{Name: "app", StartCommand: "true", DependsOn: []string{"cashmaster/refinery"}},
		{Name: "refinery", Dir: "cashmaster", StartCommand: "true", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
	}}
	snapshot := &sessionBeadSnapshot{}
	for _, b := range []beads.Bead{app, dep, blocker} {
		snapshot.addInfo(sessiontest.SeedBead(t, b))
	}
	before := time.Now().UTC().Add(-time.Second)

	refreshDesiredStateWithSessionBeads(DesiredStateResult{
		BeaconTime: before.Add(-24 * time.Hour),
		BaseState:  map[string]TemplateParams{"s-app": {TemplateName: "app", SessionName: "s-app"}},
	}, "test-city", cityPath, cfg, runtime.NewFake(), store, snapshot, io.Discard)

	got, err := store.Get(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, got.Metadata[poolAliasConflictAtMetadataKey])
	if err != nil || at.Before(before.Truncate(time.Second)) {
		t.Fatalf("pool_alias_conflict_at = %q (count %q), want the refresh time, not its day-old beacon",
			got.Metadata[poolAliasConflictAtMetadataKey], got.Metadata[poolAliasConflictCountMetadataKey])
	}
}

// The started-work gate fails closed when a leg is unreachable: a rig leg is
// PartialDegrade, so its read error leaves the walk partial rather than failed.
// The gate then cannot prove the seat holds no started work, and nothing is
// released.
func TestReleaseUnexecutedClaimsOnKill_UnreachableRigFailsClosed(t *testing.T) {
	cityPath := t.TempDir()
	seedNoRoutes(t, cityPath)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "kill-town"},
		Rigs:      []config.Rig{{Name: "riga", Path: filepath.Join(cityPath, "riga")}},
		Agents:    []config.Agent{{Name: "worker", Dir: "riga"}},
	}
	cityStore := beads.NewMemStore()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	seat, err := cityStore.Create(beads.Bead{Type: sessionBeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"template": "riga/worker", "session_name": "kill-town--worker-1", "pool_managed": "true", "pool_slot": "1",
		"state": "asleep", "sleep_reason": string(session.SleepReasonKilled), "slept_at": now.Add(-time.Minute).Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := cityStore.Create(beads.Bead{Title: "city claim", Type: "task", Assignee: seat.ID, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer

	dark := &killRaceStore{Store: beads.NewMemStore(), listErr: map[string]error{"in_progress": errors.New("rig unreachable")}}
	releaseUnexecutedClaimsOnKill(cityPath, cfg, cityStore, map[string]beads.Store{"riga": dark}, now, sessionInfosFromBeads([]beads.Bead{seat})[0], time.Minute, &stderr)

	got, err := cityStore.Get(claim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != seat.ID {
		t.Fatalf("claim released (assignee=%q) with an unreachable rig leg; stderr=%s", got.Assignee, stderr.String())
	}
}

// D5's drain-ack release keeps its identity set: the session bead's ID,
// session_name and configured identity, not the stable pool alias the kill
// path adds. A claim held under the alias is not the drain-ack's to release.
func TestDrainAckReleaseKeepsItsIdentitySet(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", NamepoolNames: []string{"ada"}, MaxActiveSessions: intPtr(2)}}}
	work := beads.NewMemStore()
	sessionBead := drainAckSessionBead()
	sessionBead.Metadata["alias"] = "ada"
	sessionBead.Metadata[poolManagedMetadataKey] = "true"
	sessionBead.Metadata["pool_slot"] = "1"
	if alias := stableAssignmentAliasForConfig(sessionBead, cfg); alias != "ada" {
		t.Fatalf("fixture alias = %q, want the stable namepool alias ada", alias)
	}
	underName := mustCreateDrainAckBead(t, work, beads.Bead{Title: "held under session_name", Type: "task"}, "in_progress", "worker-1")
	underAlias := mustCreateDrainAckBead(t, work, beads.Bead{Title: "held under alias", Type: "task"}, "in_progress", "ada")

	releaseUnexecutedClaimsOnDrainAck("", cfg, work, nil, sessionBead, time.Minute, io.Discard)

	if status, assignee := drainAckBeadStatus(t, work, underName.ID); status != "open" || assignee != "" {
		t.Fatalf("claim under session_name: status=%q assignee=%q, want released", status, assignee)
	}
	if status, assignee := drainAckBeadStatus(t, work, underAlias.ID); status != "in_progress" || assignee != "ada" {
		t.Fatalf("claim under alias: status=%q assignee=%q, want kept (D5's identity set has no alias)", status, assignee)
	}
}

// One deadline bounds the started-work gate and the release: a spent budget
// stops the gate before it reads, and nothing is released.
func TestReleaseUnexecutedClaimsOnKill_GateRunsUnderTheReleaseDeadline(t *testing.T) {
	e := newKilledSeatEnv(t, persistentWorker())
	claim := e.addWork(t, "open", e.seat.ID, routedClaim())
	reads := 0
	e.store.afterList = func(q beads.ListQuery, _ []beads.Bead) {
		if q.Status == "in_progress" {
			reads++
		}
	}
	var stderr bytes.Buffer

	releaseUnexecutedClaimsOnKill("", e.cfg, e.store, nil, e.now, sessionInfosFromBeads([]beads.Bead{e.seat})[0], -time.Second, &stderr)

	e.assertAssigned(t, claim, "open", e.seat.ID)
	if reads != 0 {
		t.Fatalf("started-work gate read %d legs past the release deadline", reads)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("budget")) {
		t.Fatalf("stderr = %q, want the exhausted-budget diagnostic", stderr.String())
	}
}
