package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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

// A park only the seat's own live read accepts ends a stranding episode too:
// the aged marker an earlier tick stamped clears, and the seat is not repaired.
func TestReconcileSessionBeads_ParkedSeatReadLiveClearsAnAgedMarker(t *testing.T) {
	env, session, pinned, rec := parkedSeatEnv(t, time.Hour, nil)
	env.setSessionMetadata(&session, map[string]string{
		strandedEventEmittedKey: env.clk.Now().Add(-strandedRepairConfirmGrace - time.Minute).UTC().Format(time.RFC3339),
	})
	other := beads.Bead{ID: "elsewhere", Status: "open", Assignee: "someone-else"}
	runParkedTick(t, env, session.ID, []beads.Bead{other}, []bool{false})
	if got := len(rec.strandedEvents()); got != 0 {
		t.Fatalf("session.stranded events = %d, want none: the live read finds only a blocked pin", got)
	}
	if got, _ := env.store.Get(session.ID); got.Status == "closed" || got.Metadata[strandedEventEmittedKey] != "" {
		t.Fatalf("session %s %v, want open with the aged marker cleared", got.Status, got.Metadata)
	}
	if p, _ := env.store.Get(pinned.ID); p.Assignee != session.ID || p.Status != "open" {
		t.Fatalf("pin %s/%q, want still open and assigned to the seat: no repair", p.Status, p.Assignee)
	}
}

// A full slot-bounded pool unparks its oldest seat, which takes the stranded
// path; once a slot is free again inside the confirm grace, the seat parks
// again, and the repair never runs (owner decision D2b). The park ends that
// stranding episode: its marker clears, so the next full pool strands the seat
// afresh, with a new event and a new grace, rather than repairing it at once
// on the first episode's aged marker.
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
	if got, _ := env.store.Get(seat.ID); got.Status == "closed" || got.Metadata[strandedEventEmittedKey] != "" {
		t.Fatalf("session %s %v, want parked again, unmarked: no repair", got.Status, got.Metadata)
	}
	if p, _ := env.store.Get(pinned.ID); p.Assignee != seat.ID {
		t.Fatalf("pin assignee %q, want its seat", p.Assignee)
	}
	if got := len(rec.strandedEvents()); got != 1 {
		t.Fatalf("session.stranded events = %d, want the one episode", got)
	}
	// The pool fills again, long past the first episode's grace.
	runParkedTick(t, env, seat.ID, []beads.Bead{pinned}, []bool{false}, busy)
	got, _ := env.store.Get(seat.ID)
	if got.Status == "closed" || got.Metadata[strandedEventEmittedKey] != env.clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("session %s %v, want open with a fresh marker: a new episode", got.Status, got.Metadata)
	}
	if p, _ := env.store.Get(pinned.ID); p.Assignee != seat.ID {
		t.Fatalf("pin assignee %q, want its seat: the new episode's grace has not passed", p.Assignee)
	}
	if got := len(rec.strandedEvents()); got != 2 {
		t.Fatalf("session.stranded events = %d, want a second episode's", got)
	}
}

// The pool-free branch's live check on a seat only eligible to park: a read
// that fails holds the seat for the tick, logged, rather than stranding it,
// but accepts no park, so no marker clears on it.
func TestHoldsParkedSeatHoldsOnAFailedRead(t *testing.T) {
	env, sess, _, _ := parkedSeatEnv(t, time.Hour, nil)
	info, err := sessionFrontDoor(env.store).Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sw := newSeatWork(testWorkLegs(env.city, env.cfg, failingReadyStore{env.store}, nil))
	var logged strings.Builder
	if held, parked := holdsParkedSeat(sw, info, parkVerdict{Eligible: true}, env.clk.Now(), &logged); !held || parked {
		t.Fatalf("holdsParkedSeat = (held %t, parked %t), want held, not parked, on a failed read", held, parked)
	}
	if !strings.Contains(logged.String(), "parked check") || !strings.Contains(logged.String(), "ready query down") {
		t.Fatalf("log %q, want the failed read reported", logged.String())
	}
	if held, parked := holdsParkedSeat(sw, info, parkVerdict{}, env.clk.Now(), &logged); held || parked {
		t.Fatalf("holdsParkedSeat = (held %t, parked %t) for a seat not eligible to park", held, parked)
	}
}

// The live check that finds the seat parked, or the verdict that already
// did, accepts the park as well as holding the seat.
func TestHoldsParkedSeatAcceptsAPark(t *testing.T) {
	env, sess, _, _ := parkedSeatEnv(t, time.Hour, nil)
	info, err := sessionFrontDoor(env.store).Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	sw := newSeatWork(testWorkLegs(env.city, env.cfg, env.store, nil))
	for _, v := range []parkVerdict{{Eligible: true}, {Eligible: true, Parked: true, Until: env.clk.Now().Add(time.Hour)}} {
		if held, parked := holdsParkedSeat(sw, info, v, env.clk.Now(), io.Discard); !held || !parked {
			t.Fatalf("holdsParkedSeat(%+v) = (held %t, parked %t), want the park accepted", v, held, parked)
		}
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
		"a parked seat a pass stranded clears its marker": func(t *testing.T, copyName string, o parityOutcome) {
			row, pin := o.rows["gc-1"], o.work["gp-1"]
			if row.Status == "closed" || row.Metadata[strandedEventEmittedKey] != "" || strandedEvents(o) != 0 {
				t.Fatalf("%s: row %s %v, %d stranded events; want parked again: open, unmarked, no event", copyName, row.Status, row.Metadata, strandedEvents(o))
			}
			if pin.Assignee != "gc-1" || pin.Status != "open" {
				t.Fatalf("%s: pin %s/%q, want open on its seat", copyName, pin.Status, pin.Assignee)
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
		"a parked seat whose ready pin loses a full city cap to its own template still wakes": func(t *testing.T, copyName string, o parityOutcome) {
			row, live := o.rows["gc-1"], o.rows["gc-2"]
			if row.Status == "closed" || row.Metadata["generation"] == "1" || strandedEvents(o) != 0 {
				t.Fatalf("%s: row %s %v, want the same row woken for its pin past the cap", copyName, row.Status, row.Metadata)
			}
			if pin := o.work["gp-1"]; pin.Assignee != "gc-1" {
				t.Fatalf("%s: pin assignee %q, want its seat", copyName, pin.Assignee)
			}
			if live.Status == "closed" || o.work["gw-2"].Assignee != "gc-2" {
				t.Fatalf("%s: live seat %s, its work on %q; want it kept on its work", copyName, live.Status, o.work["gw-2"].Assignee)
			}
		},
		"a parked seat whose ready pin loses a full city cap to another template closes as an orphan": func(t *testing.T, copyName string, o parityOutcome) {
			row, live, pin := o.rows["gc-1"], o.rows["gc-2"], o.work["gp-1"]
			if row.Status != "closed" || strandedEvents(o) != 0 {
				t.Fatalf("%s: row %s %v, %d stranded events; want the seat closed as an orphan", copyName, row.Status, row.Metadata, strandedEvents(o))
			}
			if pin.Assignee != "" || pin.Status != "open" {
				t.Fatalf("%s: pin %s/%q, want open and returned to the pool", copyName, pin.Status, pin.Assignee)
			}
			if live.Status == "closed" || o.work["gw-2"].Assignee != "gc-2" {
				t.Fatalf("%s: live seat %s, its work on %q; want it kept on its work", copyName, live.Status, o.work["gw-2"].Assignee)
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

// seedRealClockParkedSeat seeds store with a pool seat of template asleep on
// its drain, slept sleptAgo before the real clock the production passes read,
// holding one open pin blocked by an open bead. The store mints the IDs.
func seedRealClockParkedSeat(t *testing.T, store beads.Store, template string, sleptAgo time.Duration) (seatID, pinID string) {
	t.Helper()
	slept := time.Now().UTC().Add(-sleptAgo)
	row := poolRow("seat", template, 1, "asleep", "sleep_reason", "idle",
		"slept_at", slept.Format(time.RFC3339), "last_woke_at", slept.Add(-time.Hour).Format(time.RFC3339))
	row.ID = ""
	seat, err := store.Create(row)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := store.Create(beads.Bead{Title: "review loop", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	pin, err := store.Create(beads.Bead{Title: "finalize", Type: "task", Assignee: seat.ID, Metadata: map[string]string{"gc.routed_to": template}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(pin.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	return seat.ID, pin.ID
}

// checkParkedSeatPass checks a production pass over that seat: wantStranded
// session.stranded events, the stranded marker only with them, the seat open
// and asleep, and the pin still open on it.
func checkParkedSeatPass(t *testing.T, store beads.Store, seatID, pinID string, stranded []events.Event, wantStranded int) {
	t.Helper()
	if len(stranded) != wantStranded {
		t.Fatalf("session.stranded events = %d, want %d", len(stranded), wantStranded)
	}
	seat, err := store.Get(seatID)
	if err != nil {
		t.Fatal(err)
	}
	if seat.Status == "closed" || seat.Metadata["state"] != "asleep" || (seat.Metadata[strandedEventEmittedKey] != "") != (wantStranded > 0) {
		t.Fatalf("seat %s %v, want open and asleep, marked only when stranded", seat.Status, seat.Metadata)
	}
	if pin, err := store.Get(pinID); err != nil || pin.Assignee != seatID || pin.Status != "open" {
		t.Fatalf("pin %s/%q (%v), want still open on its seat", pin.Status, pin.Assignee, err)
	}
}

// The production tick (beadReconcileTick) on both of its branches: the boot
// pass decides the pool counts itself, and a steady pass reads the counts its
// demand snapshot decided, where a parked seat is no demand. Either hands the
// reconciler the verdict that holds a parked seat. The seat past its bound is
// the control: it strands on both branches and is demand, so a pass that
// never reaches the hold, or a snapshot that never parks, cannot pass. The
// boot pass's own counts stay unpinned: with headroom in the pool, a parked
// seat counted as demand changes nothing the reconciler does with it.
func TestBeadReconcileTickHoldsAParkedSeat(t *testing.T) {
	for _, tc := range []struct {
		name         string
		snapshot     bool // a steady pass over the demand snapshot's counts
		sleptAgo     time.Duration
		wantStranded int
		wantDemand   int // the demand snapshot's worker count, on a steady pass
	}{
		{name: "the boot pass", sleptAgo: time.Hour},
		{name: "a steady pass", snapshot: true, sleptAgo: time.Hour},
		{name: "the boot pass, past the bound", sleptAgo: parkedWorkMaxHold + time.Hour, wantStranded: 1},
		{name: "a steady pass, past the bound", snapshot: true, sleptAgo: parkedWorkMaxHold + time.Hour, wantStranded: 1, wantDemand: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
			store := beads.NewMemStore()
			seatID, pinID := seedRealClockParkedSeat(t, store, "worker", tc.sleptAgo)
			rec := events.NewFake()
			cfg := workerCity(3)
			cr := &CityRuntime{
				cityPath: t.TempDir(), cityName: cfg.Workspace.Name, cfg: cfg, sp: runtime.NewFake(),
				standaloneCityStore: store, sessionDrains: newDrainTracker(),
				rec: rec, stdout: io.Discard, stderr: io.Discard,
			}
			cr.buildFnWithSessionBeads = supervisorBuildAgentsFnWithSessionBeads(cr.cityPath, cr.cityName, io.Discard)
			t.Cleanup(func() { cr.asyncStarts.wait(-1); cr.asyncStops.wait(-1) })
			snap, err := loadSessionBeadSnapshot(store)
			if err != nil {
				t.Fatal(err)
			}
			if tc.snapshot {
				result := cr.loadDemandSnapshot(snap, nil, "poke", false).result
				if result.PoolDesiredCounts == nil {
					t.Fatal("the demand snapshot decided no pool counts")
				}
				if got := result.PoolDesiredCounts["worker"]; got != tc.wantDemand {
					t.Fatalf("the demand snapshot counted %d worker sessions, want %d", got, tc.wantDemand)
				}
				cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, snap, nil, false)
			} else {
				result := cr.buildDesiredState(snap, nil)
				if result.PoolDesiredCounts != nil {
					t.Fatalf("the boot build decided pool counts %v, want them left to the tick", result.PoolDesiredCounts)
				}
				cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, snap, nil, true)
			}

			stranded, err := rec.List(events.Filter{Type: events.SessionStranded})
			if err != nil {
				t.Fatal(err)
			}
			checkParkedSeatPass(t, store, seatID, pinID, stranded, tc.wantStranded)
		})
	}
}

// The control-dispatcher tick (controlDispatcherTick) holds a parked
// dispatcher seat with the verdict its pool counts read: the counts drop the
// seat from demand, and the reconciler of the same pass holds it rather than
// stranding it. The seat past its bound is the control: it strands, so a pass
// that never reaches the seat cannot pass.
func TestControlDispatcherTickHoldsAParkedSeat(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sleptAgo     time.Duration
		wantStranded int
	}{
		{name: "parked", sleptAgo: time.Hour},
		{name: "past the bound", sleptAgo: parkedWorkMaxHold + time.Hour, wantStranded: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
			store := beads.NewMemStore()
			seatID, pinID := seedRealClockParkedSeat(t, store, config.ControlDispatcherAgentName, tc.sleptAgo)
			rec := events.NewFake()
			cfg := &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents: []config.Agent{{
					Name:              config.ControlDispatcherAgentName,
					StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
					MaxActiveSessions: intPtr(3),
				}},
			}
			sp := runtime.NewFake()
			cr := &CityRuntime{
				cityPath: t.TempDir(), cityName: cfg.Workspace.Name, cfg: cfg, sp: sp, dops: newDrainOps(sp),
				standaloneCityStore: store, sessionDrains: newDrainTracker(),
				rec: rec, stdout: io.Discard, stderr: io.Discard,
				// No managed Dolt here: the preflight must not exec the beads script.
				managedDoltHealth: func(context.Context, string) error { return nil },
			}
			cr.buildFnWithSessionBeads = supervisorBuildAgentsFnWithSessionBeads(cr.cityPath, cr.cityName, io.Discard)
			t.Cleanup(func() { cr.asyncStarts.wait(-1); cr.asyncStops.wait(-1) })

			cr.controlDispatcherTick(context.Background())

			stranded, err := rec.List(events.Filter{Type: events.SessionStranded})
			if err != nil {
				t.Fatal(err)
			}
			checkParkedSeatPass(t, store, seatID, pinID, stranded, tc.wantStranded)
		})
	}
}

// The one-shot start (doStartStandalone, cmd_start.go) holds a parked seat
// with the verdict its pool counts read. The seat past its bound is the
// control. Its counts stay unpinned, as the boot tick's do.
func TestStartStandaloneHoldsAParkedSeat(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sleptAgo     time.Duration
		wantStranded int
	}{
		{name: "parked", sleptAgo: time.Hour},
		{name: "past the bound", sleptAgo: parkedWorkMaxHold + time.Hour, wantStranded: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
			cityPath := t.TempDir()
			clearInheritedBeadsEnv(t)
			t.Chdir(t.TempDir())
			cityTOML := `[workspace]
name = "parked-seat-city"
provider = "shell"

[providers.shell]
command = "echo"

[beads]
provider = "file"

[[agent]]
name = "worker"
scope = "city"
max_active_sessions = 3
`
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := ensureScopedFileStoreLayout(cityPath); err != nil {
				t.Fatal(err)
			}
			if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
				t.Fatal(err)
			}
			store, err := openScopeLocalFileStore(cityPath)
			if err != nil {
				t.Fatal(err)
			}
			seatID, pinID := seedRealClockParkedSeat(t, store, "worker", tc.sleptAgo)
			oldBuild := buildSessionProviderByName
			t.Cleanup(func() { buildSessionProviderByName = oldBuild })
			buildSessionProviderByName = func(_ *config.City, _ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
				return runtime.NewFake(), nil
			}

			var stdout, stderr bytes.Buffer
			if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 0 {
				t.Fatalf("doStartStandalone exit = %d\nstderr:\n%s", code, stderr.String())
			}
			stranded, err := events.ReadFiltered(filepath.Join(cityPath, ".gc", "events.jsonl"), events.Filter{Type: events.SessionStranded})
			if err != nil {
				t.Fatal(err)
			}
			checkParkedSeatPass(t, store, seatID, pinID, stranded, tc.wantStranded)
		})
	}
}
