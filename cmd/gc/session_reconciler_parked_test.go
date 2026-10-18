package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// The legacy reconciler's half of a parked seat (ga-ylpbsy.16): a pool seat
// asleep on its own drain holding only a pre-assigned pin that is blocked is
// held, with no session.stranded and no repair, however long it waits inside
// its bound. Anything else strands as before.

// parkedSeatEnv is a desired, not-running pool seat asleep on its drain
// (sleep_reason idle) slept sleptAgo before the clock, holding one open pin
// blocked by an open bead.
func parkedSeatEnv(t *testing.T, sleptAgo time.Duration, meta map[string]string) (*reconcilerTestEnv, beads.Bead, beads.Bead, *capturingRecorder) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	m := map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		"slept_at":             env.clk.Now().Add(-sleptAgo).UTC().Format(time.RFC3339),
		poolManagedMetadataKey: boolMetadata(true),
	}
	for k, v := range meta {
		m[k] = v
	}
	env.setSessionMetadata(&session, m)
	blocker, err := env.store.Create(beads.Bead{Title: "review loop", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := env.store.Create(beads.Bead{
		Title: "finalize", Type: "task", Status: "open", Assignee: session.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.DepAdd(pinned.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	rec := &capturingRecorder{}
	env.rec = rec
	return env, session, pinned, rec
}

// runParkedTick runs one legacy tick with the controller's assigned-work view
// of work and its readiness, and the park verdicts the controller decides over
// that view (ready nil: no readiness verdict, so none).
func runParkedTick(t *testing.T, env *reconcilerTestEnv, sessionID string, work []beads.Bead, ready []bool, others ...session.Info) {
	t.Helper()
	b, err := env.store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := sessionFrontDoor(env.store).Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// others are the pool's other open rows, which the verdict's slot
	// headroom counts.
	parked := parkRows(env.cfg, append([]session.Info{info}, others...), work, ready, env.clk.Now().UTC())
	reconcileSessionBeadsAtPath(context.Background(), env.city, []beads.Bead{b}, env.desiredState, map[string]bool{"worker": true},
		env.cfg, env.sp, env.store, newFakeDrainOps(), work, nil, nil, env.dt, nil, false, nil, "", nil, env.clk, env.rec, 0, 0,
		&env.stdout, &env.stderr, withAsyncDrainAckStopTracker(env.stops), withReadyAssignedFlags(ready), withParkedSeats(parked))
}

func TestReconcileSessionBeads_ParkedSeatIsNotStranded(t *testing.T) {
	env, session, pinned, rec := parkedSeatEnv(t, time.Hour, nil)
	for tick := range 3 {
		runParkedTick(t, env, session.ID, []beads.Bead{pinned}, []bool{false})
		env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
		if got := len(rec.strandedEvents()); got != 0 {
			t.Fatalf("tick %d: session.stranded events = %d, want none for a parked seat", tick, got)
		}
	}
	got, _ := env.store.Get(session.ID)
	if got.Status == "closed" || got.Metadata[strandedEventEmittedKey] != "" {
		t.Fatalf("session %s %v, want open and unmarked", got.Status, got.Metadata)
	}
	if p, _ := env.store.Get(pinned.ID); p.Assignee != session.ID || p.Status != "open" {
		t.Fatalf("pin %s/%q, want still open and assigned to the seat", p.Status, p.Assignee)
	}
}

// The tick's view may miss the pin; the seat's own live read decides.
func TestReconcileSessionBeads_ParkedSeatReadLiveWhenTheViewMissesItsPin(t *testing.T) {
	env, session, pinned, rec := parkedSeatEnv(t, time.Hour, nil)
	other := beads.Bead{ID: "elsewhere", Status: "open", Assignee: "someone-else"}
	for range 2 {
		runParkedTick(t, env, session.ID, []beads.Bead{other}, []bool{false})
		env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
	}
	if got := len(rec.strandedEvents()); got != 0 {
		t.Fatalf("session.stranded events = %d, want none: the live read finds only a blocked pin", got)
	}
	if got, _ := env.store.Get(session.ID); got.Status == "closed" || got.Metadata[strandedEventEmittedKey] != "" {
		t.Fatalf("session %s %v, want open and unmarked", got.Status, got.Metadata)
	}
	if p, _ := env.store.Get(pinned.ID); p.Assignee != session.ID {
		t.Fatalf("pin assignee %q, want its seat", p.Assignee)
	}
}

// A full slot-bounded pool unparks its oldest seat, which takes the stranded
// path; once a slot is free again inside the confirm grace, the seat parks
// again, and the repair never runs (owner decision D2b).
func TestReconcileSessionBeads_UnparkedSeatParksAgainWhenASlotFrees(t *testing.T) {
	env, seat, pinned, rec := parkedSeatEnv(t, time.Hour, nil)
	env.cfg.Agents[0].MaxActiveSessions = intPtr(2)
	busy := session.Info{ID: "busy", Template: "worker", PoolManaged: true, PoolSlot: "2", MetadataState: "awake", State: session.StateActive}
	runParkedTick(t, env, seat.ID, []beads.Bead{pinned}, []bool{false}, busy)
	if got := len(rec.strandedEvents()); got != 1 {
		t.Fatalf("session.stranded events = %d, want 1: a full pool unparks its oldest seat", got)
	}
	// The busy seat closes: the pool has a free slot again.
	for range 2 {
		env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
		runParkedTick(t, env, seat.ID, []beads.Bead{pinned}, []bool{false})
	}
	if got, _ := env.store.Get(seat.ID); got.Status == "closed" {
		t.Fatalf("session closed (%v), want parked again: no repair", got.Metadata)
	}
	if p, _ := env.store.Get(pinned.ID); p.Assignee != seat.ID {
		t.Fatalf("pin assignee %q, want its seat", p.Assignee)
	}
	if got := len(rec.strandedEvents()); got != 1 {
		t.Fatalf("session.stranded events = %d, want the one episode", got)
	}
}

// The pool-free branch's live check on a seat only eligible to park: a read
// that fails holds the seat for the tick, logged, rather than stranding it.
func TestHoldsParkedSeatHoldsOnAFailedRead(t *testing.T) {
	env, sess, _, _ := parkedSeatEnv(t, time.Hour, nil)
	info, err := sessionFrontDoor(env.store).Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sw := newSeatWork(testWorkLegs(env.city, env.cfg, failingReadyStore{env.store}, nil))
	var logged strings.Builder
	if !holdsParkedSeat(sw, info, parkVerdict{Eligible: true}, env.clk.Now(), &logged) {
		t.Fatal("holdsParkedSeat = false, want the seat held on a failed read")
	}
	if !strings.Contains(logged.String(), "parked check") || !strings.Contains(logged.String(), "ready query down") {
		t.Fatalf("log %q, want the failed read reported", logged.String())
	}
	if holdsParkedSeat(sw, info, parkVerdict{}, env.clk.Now(), &logged) {
		t.Fatal("holdsParkedSeat = true for a seat not eligible to park")
	}
}

func TestReconcileSessionBeads_StrandsWhatIsNotParked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sleptAgo time.Duration
		meta     map[string]string
		ready    []bool
		started  bool // add an in-progress claim beside the pin
		noView   bool // a caller with no readiness verdict
	}{
		{name: "a seat past its bound", sleptAgo: parkedWorkMaxHold + time.Minute, ready: []bool{false}},
		{name: "a seat whose runtime died (runtime-missing)", sleptAgo: time.Hour, meta: map[string]string{"sleep_reason": "runtime-missing"}, ready: []bool{false}},
		{name: "a fresh-wake seat", sleptAgo: time.Hour, meta: map[string]string{"wake_mode": "fresh"}, ready: []bool{false}},
		{name: "a seat holding a started claim", sleptAgo: time.Hour, ready: []bool{false, false}, started: true},
		{name: "a pass with no readiness verdict", sleptAgo: time.Hour, noView: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, pinned, rec := parkedSeatEnv(t, tc.sleptAgo, tc.meta)
			work := []beads.Bead{pinned}
			if tc.started {
				claim, err := env.store.Create(beads.Bead{Title: "claimed", Type: "task", Status: "open", Assignee: session.ID})
				if err != nil {
					t.Fatal(err)
				}
				inProgress := "in_progress"
				if err := env.store.Update(claim.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
					t.Fatal(err)
				}
				claim, _ = env.store.Get(claim.ID)
				// Blocked, so it is no wake demand: the seat stays asleep
				// holding a started claim, the stranding the event is for.
				blocked := true
				claim.IsBlocked = &blocked
				work = append(work, claim)
			}
			ready := tc.ready
			if tc.noView {
				ready = nil
			}
			runParkedTick(t, env, session.ID, work, ready)
			if got := len(rec.strandedEvents()); got != 1 {
				t.Fatalf("session.stranded events = %d, want 1", got)
			}
			env.clk.Time = env.clk.Time.Add(strandedRepairConfirmGrace + time.Minute)
			runParkedTick(t, env, session.ID, work, ready)
			if got, _ := env.store.Get(session.ID); got.Status != "closed" {
				t.Fatalf("session %s, want closed by the stranded repair", got.Status)
			}
			if p, _ := env.store.Get(pinned.ID); p.Assignee != "" {
				t.Fatalf("pin assignee %q, want released by the repair", p.Assignee)
			}
		})
	}
}

// The parked fixtures' end states, asserted on both copies: parity alone
// would pass two copies wrong the same way.
func TestParkedSeatFixturesEndStates(t *testing.T) {
	strandedEvents := func(o parityOutcome) int {
		n := 0
		for k, c := range o.events {
			if strings.HasPrefix(k, "session.stranded ") {
				n += c
			}
		}
		return n
	}
	for name, check := range map[string]func(t *testing.T, copyName string, o parityOutcome){
		"a seat asleep on its drain holding only a blocked pin stays parked": func(t *testing.T, copyName string, o parityOutcome) {
			row, pin := o.rows["gc-1"], o.work["gp-1"]
			if row.Status == "closed" || row.Metadata["state"] != "asleep" || row.Metadata[strandedEventEmittedKey] != "" || strandedEvents(o) != 0 {
				t.Fatalf("%s: row %s %v, %d stranded events; want parked: asleep, open, unmarked", copyName, row.Status, row.Metadata, strandedEvents(o))
			}
			// Its conversation is kept for the resume the wake will make.
			if row.Metadata["session_key"] != "conversation-1" {
				t.Fatalf("%s: session_key %q, want the parked seat's conversation kept", copyName, row.Metadata["session_key"])
			}
			if pin.Assignee != "gc-1" || pin.Status != "open" {
				t.Fatalf("%s: pin %s/%q, want open on its seat", copyName, pin.Status, pin.Assignee)
			}
		},
		"a parked seat past its bound strands": func(t *testing.T, copyName string, o parityOutcome) {
			if strandedEvents(o) != 1 || o.rows["gc-1"].Metadata[strandedEventEmittedKey] == "" {
				t.Fatalf("%s: %d stranded events, row %v; want the seat stranded", copyName, strandedEvents(o), o.rows["gc-1"].Metadata)
			}
		},
		"a parked seat whose pin is ready wakes in place": func(t *testing.T, copyName string, o parityOutcome) {
			row := o.rows["gc-1"]
			if row.Status == "closed" || row.Metadata["generation"] == "1" || strandedEvents(o) != 0 {
				t.Fatalf("%s: row %s %v, want the same row woken for its pin", copyName, row.Status, row.Metadata)
			}
			if pin := o.work["gp-1"]; pin.Assignee != "gc-1" {
				t.Fatalf("%s: pin assignee %q, want its seat", copyName, pin.Assignee)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			i := slices.IndexFunc(parityFixtures(), func(f parityFixture) bool { return f.Name == name })
			if i < 0 {
				t.Fatalf("no fixture %q", name)
			}
			f := parityFixtures()[i]
			cityPath := t.TempDir()
			rows := f.converged(t, cityPath)
			check(t, "legacy", legacyParity(t, f, cityPath, rows))
			check(t, "v2", v2Parity(t, f, cityPath, rows))
		})
	}
}
