package main

import (
	"context"
	"io"
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

// Killed pool seats (owner ruling B1, CONTRACT v5.7 C3 "Killed pool seats",
// §12.2 row 28, SESS-623/625). A pool row `gc session kill` stopped, with no
// honored kill fence, frees its slot: its unexecuted open claims are released
// first, started work keeps the seat (woken in place while the work has demand,
// asleep while it is blocked), and it never takes the stranded branch.

// killedSeatEnv is a persistent pool (min 1, max 2) whose only seat, slot 1,
// was stopped by `gc session kill`: asleep, sleep_reason=killed, the kill fence
// already lifted, runtime gone.
type killedSeatEnv struct {
	now   time.Time
	city  string
	cfg   *config.City
	store beads.Store
	sp    *runtime.Fake
	clk   *clock.Fake
	dt    *drainTracker
	seat  beads.Bead
}

func newKilledSeatEnv(t *testing.T) *killedSeatEnv {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "kill-town", "worker")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "kill-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "repo",
			StartCommand:      "true",
			MinActiveSessions: intPtr(1),
			MaxActiveSessions: intPtr(2),
		}},
	}
	store := beads.NewMemStore()
	seat := createCanonicalPoolSession(t, store, &cfg.Agents[0], now.Add(-time.Hour), 1)
	if err := store.SetMetadataBatch(seat.ID, map[string]string{
		"state":                     "asleep",
		"sleep_reason":              string(session.SleepReasonKilled),
		"slept_at":                  now.Add(-time.Minute).Format(time.RFC3339),
		"pending_create_claim":      "",
		"pending_create_started_at": "",
	}); err != nil {
		t.Fatalf("mark seat killed: %v", err)
	}
	seat, err := store.Get(seat.ID)
	if err != nil {
		t.Fatalf("reload seat: %v", err)
	}
	return &killedSeatEnv{
		now:   now,
		city:  cityDir,
		cfg:   cfg,
		store: store,
		sp:    runtime.NewFake(),
		clk:   &clock.Fake{Time: now},
		dt:    newDrainTracker(),
		seat:  seat,
	}
}

// addWork creates a work bead assigned to the seat with the given status.
func (e *killedSeatEnv) addWork(t *testing.T, status string) beads.Bead {
	t.Helper()
	work, err := e.store.Create(beads.Bead{Title: "work " + status, Type: "task", Status: "open", Assignee: e.seat.ID})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if status != "open" {
		if err := e.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("set work status %s: %v", status, err)
		}
	}
	work, err = e.store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work: %v", err)
	}
	return work
}

// tick runs one legacy reconcile pass over the seat. assigned is the tick's
// actionable assigned-work snapshot, the input AL1's assigned-work pass reads.
func (e *killedSeatEnv) tick(t *testing.T, assigned []beads.Bead) {
	t.Helper()
	seat, err := e.store.Get(e.seat.ID)
	if err != nil {
		t.Fatalf("reload seat: %v", err)
	}
	ds := buildDesiredState("kill-town", e.city, e.now, e.cfg, e.sp, e.store, io.Discard)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{seat}, ds.State, map[string]bool{"repo/worker": true},
		e.cfg, e.sp, e.store, newFakeDrainOps(), assigned, nil, e.dt, ds.PoolDesiredCounts, false, nil, "kill-town",
		nil, e.clk, events.Discard, 0, 0, io.Discard, io.Discard,
	)
}

func (e *killedSeatEnv) reload(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := e.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// openWorkerSeats lists the open session beads of the worker pool.
func (e *killedSeatEnv) openWorkerSeats(t *testing.T) []beads.Bead {
	t.Helper()
	all, err := e.store.List(beads.ListQuery{Type: sessionBeadType})
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

func assertNoStrandedResidue(t *testing.T, seat beads.Bead) {
	t.Helper()
	if got := seat.Metadata[strandedEventEmittedKey]; got != "" {
		t.Fatalf("killed seat took the stranded branch: %s=%q", strandedEventEmittedKey, got)
	}
}

// J39: an idle killed persistent seat closes (close_reason killed) and the
// pool's floor creates a fresh seat with a fresh identity in its slot. Legacy
// kept the killed row open and asleep, holding the slot for good.
func TestReconcileSessionBeads_KilledIdlePoolSeatClosesAndIsReplaced(t *testing.T) {
	e := newKilledSeatEnv(t)

	e.tick(t, nil)

	assertClosedAsKilled(t, e.reload(t, e.seat.ID))

	// The next desired-state build realizes the floor with a fresh seat.
	buildDesiredState("kill-town", e.city, e.now, e.cfg, e.sp, e.store, io.Discard)
	open := e.openWorkerSeats(t)
	if len(open) != 1 {
		t.Fatalf("open worker seats after close = %d, want 1 fresh seat: %+v", len(open), open)
	}
	if open[0].ID == e.seat.ID {
		t.Fatalf("replacement seat reused the killed row %s; want a fresh identity", e.seat.ID)
	}
}

// Rule 2: unexecuted claims (open, no execution evidence) are released before
// the close, then the seat closes. Open work the row names as its current claim
// has execution evidence and is not released, so it keeps the seat.
func TestReconcileSessionBeads_KilledPoolSeatReleasesUnstartedWorkThenCloses(t *testing.T) {
	e := newKilledSeatEnv(t)
	unstarted := e.addWork(t, "open")

	// Not ready, so the snapshot carries no wake demand for it.
	e.tick(t, nil)

	work := e.reload(t, unstarted.ID)
	if work.Assignee != "" || work.Status != "open" {
		t.Fatalf("unstarted work after kill close: status=%q assignee=%q, want open and unassigned", work.Status, work.Assignee)
	}
	assertClosedAsKilled(t, e.reload(t, e.seat.ID))
}

func TestReconcileSessionBeads_KilledPoolSeatKeepsOpenWorkWithExecutionEvidence(t *testing.T) {
	for _, key := range []string{session.CurrentBeadIDKey, beadmeta.CurrentClaimBeadIDMetadataKey} {
		t.Run(key, func(t *testing.T) {
			e := newKilledSeatEnv(t)
			claimed := e.addWork(t, "open")
			other := e.addWork(t, "open")
			if err := e.store.SetMetadata(e.seat.ID, key, claimed.ID); err != nil {
				t.Fatalf("stamp %s: %v", key, err)
			}

			e.tick(t, nil)

			if w := e.reload(t, claimed.ID); w.Assignee != e.seat.ID {
				t.Fatalf("work named by %s was released (assignee=%q); execution evidence must keep it", key, w.Assignee)
			}
			if w := e.reload(t, other.ID); w.Assignee != "" {
				t.Fatalf("unstarted sibling assignee = %q, want released", w.Assignee)
			}
			got := e.reload(t, e.seat.ID)
			if got.Status == "closed" {
				t.Fatal("seat closed while holding work with execution evidence")
			}
			assertNoStrandedResidue(t, got)
		})
	}
}

// Rule 1: a killed seat holding unblocked in_progress work is woken in place on
// its bead by AL1's assigned-work pass; nothing is released and the row is not
// closed.
func TestReconcileSessionBeads_KilledPoolSeatWithStartedWorkWakesInPlace(t *testing.T) {
	e := newKilledSeatEnv(t)
	started := e.addWork(t, "in_progress")
	unstarted := e.addWork(t, "open")

	e.tick(t, []beads.Bead{started})

	got := e.reload(t, e.seat.ID)
	if got.Status == "closed" {
		t.Fatal("killed seat with started work was closed; want it kept and woken in place")
	}
	if !e.sp.IsRunning(got.Metadata["session_name"]) {
		t.Fatalf("killed seat with started work not woken: state=%q sleep_reason=%q", got.Metadata["state"], got.Metadata["sleep_reason"])
	}
	if w := e.reload(t, started.ID); w.Status != "in_progress" || w.Assignee != e.seat.ID {
		t.Fatalf("started work status=%q assignee=%q, want in_progress on the seat", w.Status, w.Assignee)
	}
	// A woken seat is not closing, so even its unstarted claim stays with it.
	if w := e.reload(t, unstarted.ID); w.Assignee != e.seat.ID {
		t.Fatalf("unstarted work on a woken seat was released (assignee=%q)", w.Assignee)
	}
	assertNoStrandedResidue(t, got)
}

// Rule 1 and 3: a killed seat whose started work is blocked has no wake
// demand, so the close runs; it releases nothing in_progress, the live work
// read finds the work and the close refuses, and the seat holds its slot
// asleep. It never takes the stranded branch, even past the stranded repair's
// confirmation window, which would hand the half-done bead to a fresh seat.
func TestReconcileSessionBeads_KilledPoolSeatWithBlockedStartedWorkHoldsSlotAsleep(t *testing.T) {
	e := newKilledSeatEnv(t)
	started := e.addWork(t, "in_progress")
	blocked := true
	started.IsBlocked = &blocked

	e.tick(t, []beads.Bead{started})
	e.clk.Time = e.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
	e.now = e.clk.Time
	e.tick(t, []beads.Bead{started})

	got := e.reload(t, e.seat.ID)
	if got.Status == "closed" {
		t.Fatalf("killed seat with blocked started work closed (close_reason=%q); want it to hold its slot", got.Metadata["close_reason"])
	}
	if got.Metadata["state"] != "asleep" || got.Metadata["sleep_reason"] != string(session.SleepReasonKilled) {
		t.Fatalf("seat state=%q sleep_reason=%q, want asleep/killed", got.Metadata["state"], got.Metadata["sleep_reason"])
	}
	if e.sp.IsRunning(got.Metadata["session_name"]) {
		t.Fatal("seat woken for blocked work")
	}
	if w := e.reload(t, started.ID); w.Status != "in_progress" || w.Assignee != e.seat.ID {
		t.Fatalf("blocked started work status=%q assignee=%q, want in_progress on the seat", w.Status, w.Assignee)
	}
	assertNoStrandedResidue(t, got)
}

// An honored kill fence means the kill still owns the row: not freeable, so
// neither the release nor the close runs.
func TestReconcileSessionBeads_KilledPoolSeatWithPendingKillFenceIsNotFreed(t *testing.T) {
	e := newKilledSeatEnv(t)
	if err := e.store.SetMetadata(e.seat.ID, "state_reason", session.KillPendingReason); err != nil {
		t.Fatalf("stamp kill fence: %v", err)
	}
	unstarted := e.addWork(t, "open")

	e.tick(t, nil)

	got := e.reload(t, e.seat.ID)
	if got.Status == "closed" {
		t.Fatal("seat closed under an honored kill fence")
	}
	if w := e.reload(t, unstarted.ID); w.Assignee != e.seat.ID {
		t.Fatalf("work released under an honored kill fence (assignee=%q)", w.Assignee)
	}
}

// A one_shot pool reuses an idle killed seat in place rather than closing it;
// a fenced or work-holding killed seat is not reused, and a persistent pool
// never reuses an asleep row.
func TestReusablePoolSessionInfo_OneShotKilledIdleSeatIsReusable(t *testing.T) {
	t.Parallel()

	bp := &agentBuildParams{}
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
		SleptAt:             time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
	}
	if !reusablePoolSessionInfo(bp, oneShot, template, killed, nil) {
		t.Fatal("one_shot idle killed seat not reusable; want reuse in place")
	}

	fenced := killed
	fenced.StateReason = session.KillPendingReason
	if reusablePoolSessionInfo(bp, oneShot, template, fenced, nil) {
		t.Fatal("one_shot killed seat under an honored kill fence reused")
	}

	withWork := &agentBuildParams{assignedWorkBeads: []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: killed.ID}}}
	if reusablePoolSessionInfo(withWork, oneShot, template, killed, nil) {
		t.Fatal("one_shot killed seat holding work reused; rules 1-3 must apply to it instead")
	}

	persistent := &config.Agent{Name: "worker", Dir: "repo"}
	if reusablePoolSessionInfo(bp, persistent, template, killed, nil) {
		t.Fatal("persistent pool reused an asleep killed row")
	}
}
