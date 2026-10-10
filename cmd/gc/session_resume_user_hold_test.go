package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// J27 A: `gc session attach` of a session `gc session suspend` held and
// drained is an operator's own resume. It consumes the hold, so legacy's
// controller does not drain the attached session again. Real legacy ticks,
// with the production drain ops persisted across them.

// heldDrainedSession is a desired session the managed suspend held, whose
// drain the agent acked and whose runtime is gone: the row J27 A attaches to.
func heldDrainedSession(t *testing.T) (*reconcilerTestEnv, beads.Bead, drainOps) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	b := env.createSessionBead("worker", "worker")
	env.markSessionActive(&b)
	env.clk.Time = env.clk.Time.Add(30 * time.Minute)
	front := sessionFrontDoor(env.store)
	if err := front.ApplyPatch(b.ID, session.OperatorSuspendPatch(env.clk.Now())); err != nil {
		t.Fatal(err)
	}
	if err := env.sp.Stop("worker"); err != nil {
		t.Fatal(err)
	}
	if err := front.ApplyPatch(b.ID, session.AcknowledgeDrainPatch(env.clk.Now(), false)); err != nil {
		t.Fatal(err)
	}
	return env, b, newDrainOps(env.sp)
}

func (e *reconcilerTestEnv) userHoldTick(t *testing.T, id string, dops drainOps) {
	t.Helper()
	e.userHoldTickAt(t, e.city, id, dops)
}

// userHoldTickAt is userHoldTick for a city at cityPath: the controller's
// kills take the runtime lease there.
func (e *reconcilerTestEnv) userHoldTickAt(t *testing.T, cityPath, id string, dops drainOps, opts ...startExecutionOption) {
	t.Helper()
	got, err := e.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	e.userHoldTickOn(t, cityPath, got, dops, opts...)
}

// userHoldTickOn runs one legacy tick over a given snapshot of the row: a
// pass that read the row earlier.
func (e *reconcilerTestEnv) userHoldTickOn(t *testing.T, cityPath string, got beads.Bead, dops drainOps, opts ...startExecutionOption) {
	t.Helper()
	reconcileSessionBeadsAtPath(
		context.Background(), cityPath, []beads.Bead{got}, e.desiredState,
		configuredSessionNames(e.cfg, "", e.store), e.cfg, e.sp, e.store,
		dops, nil, nil, nil, e.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, e.clk, e.rec, 0, 0, &e.stdout, &e.stderr, append(append([]startExecutionOption{}, e.startOptions...), opts...)...,
	)
}

// assertResumedStaysUp runs n ticks a drain timeout apart and fails if any
// stops the resumed runtime, or the hold or a drain survives them.
func (e *reconcilerTestEnv) assertResumedStaysUp(t *testing.T, id string, dops drainOps, n int) {
	t.Helper()
	stops := e.sp.CountCalls("Stop", "worker")
	for i := 0; i < n; i++ {
		e.clk.Time = e.clk.Time.Add(defaultDrainTimeout + time.Minute)
		e.userHoldTick(t, id, dops)
		if !e.sp.IsRunning("worker") || e.sp.CountCalls("Stop", "worker") != stops {
			t.Fatalf("tick %d stopped the resumed runtime; stdout:\n%s", i+1, e.stdout.String())
		}
	}
	info := e.sessionInfo(id)
	if info.SleepIntent != "" || info.HeldUntil != "" || e.dt.get(id) != nil {
		t.Fatalf("row sleep_intent=%q held_until=%q drain=%+v, want the hold consumed and no drain", info.SleepIntent, info.HeldUntil, e.dt.get(id))
	}
}

func TestAttachAfterManagedSuspendStaysUp(t *testing.T) {
	env, b, dops := heldDrainedSession(t)
	mgr := session.NewManagerWithOptions(env.store, env.sp, session.WithClock(env.clk), session.WithCityPath(env.city))
	if err := mgr.Attach(context.Background(), b.ID, "test-cmd", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	env.assertResumedStaysUp(t, b.ID, dops, 6)
}

// A user-hold drain the controller still tracks, past its deadline, is
// released once a resume consumed the hold: no forced stop, no completion
// written over the resumed row. A drain whose row still holds the intent
// runs as before.
func TestUserHoldDrainReleasedOnceTheHoldIsConsumed(t *testing.T) {
	testCity := t.TempDir()
	for intent, wantStop := range map[string]bool{"": false, "user-hold": true} {
		now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "s-held", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "s-held", "template": "worker", "generation": "1", "state": "active", "sleep_intent": intent,
		}})
		if err != nil {
			t.Fatal(err)
		}
		dt := newDrainTracker()
		beginDrainForTest(t, store, dt, b.ID, "user-hold", now.Add(-time.Hour), now.Add(-time.Minute)).ackSet = true
		advanceSessionDrainsWithSessionsTraced(testCity, dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
			got, _ := store.Get(id)
			return &got
		}), map[string]wakeEvaluation{}, &config.City{}, &clock.Fake{Time: now}, nil)
		if stopped := sp.CountCalls("Stop", "s-held") > 0; stopped != wantStop || dt.get(b.ID) != nil {
			t.Errorf("sleep_intent %q: stopped=%v tracked=%v, want stopped=%v and the drain done", intent, stopped, dt.get(b.ID) != nil, wantStop)
		}
	}
}

// tickOnStart runs one legacy tick right after the provider Start call,
// before the resume confirms it: the mid-resume race.
type tickOnStart struct {
	*runtime.Fake
	tick func()
}

func (p *tickOnStart) Start(ctx context.Context, name string, cfg runtime.Config) error {
	err := p.Fake.Start(ctx, name, cfg)
	if tick := p.tick; tick != nil {
		p.tick = nil
		tick()
	}
	return err
}

// A tick between the resume's Start and its consume sees a held row with a
// live runtime, so it begins a user-hold drain and publishes its ack. The
// consume still lands (the tick wrote nothing it reads), and the next ticks
// release that drain and its ack instead of honoring them.
func TestAttachAfterManagedSuspendSurvivesAMidResumeTick(t *testing.T) {
	env, b, dops := heldDrainedSession(t)
	sp := &tickOnStart{Fake: env.sp}
	sp.tick = func() {
		env.userHoldTick(t, b.ID, dops)
		if ds := env.dt.get(b.ID); ds == nil || ds.reason != "user-hold" {
			t.Errorf("mid-resume tick drain = %+v, want a user-hold drain (test premise)", ds)
		}
	}
	mgr := session.NewManagerWithOptions(env.store, sp, session.WithClock(env.clk), session.WithCityPath(env.city))
	if err := mgr.Attach(context.Background(), b.ID, "test-cmd", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if sp.tick != nil {
		t.Fatal("the mid-resume tick did not run (test premise)")
	}
	env.assertResumedStaysUp(t, b.ID, dops, 6)
}

// The reviewer's race: a tick and its follow-up both run while the operator's
// attach is inside the provider Start. The first begins the user-hold drain
// and publishes its ack; the second would honor the ack, but the stop-pending
// marker, like the kill, is decided under the runtime lease, which the attach
// holds through Start and its consume: the marker defers and no stop is
// queued. The consume then lands, and later ticks find the ack stale.
func TestAttachSurvivesATickAndItsFollowUpDuringStart(t *testing.T) {
	env, b, dops := heldDrainedSession(t)
	city := t.TempDir()
	sp := &tickOnStart{Fake: env.sp}
	sp.tick = func() {
		stops := &asyncStartTracker{}
		env.userHoldTickAt(t, city, b.ID, dops, withAsyncDrainAckStopTracker(stops))
		env.clk.Time = env.clk.Time.Add(time.Minute)
		env.userHoldTickAt(t, city, b.ID, dops, withAsyncDrainAckStopTracker(stops))
		if info := env.sessionInfo(b.ID); info.StateReason == session.DrainAckStopPendingReason ||
			!strings.Contains(env.stderr.String(), "stop-pending worker deferred") {
			t.Errorf("follow-up tick left reason=%q; stderr=%q; want the marker deferred on the attach's lease", info.StateReason, env.stderr.String())
		}
		waitAsyncStopsForTest(t, stops) // any queued stop runs to completion inside the Start window
		if !env.sp.IsRunning("worker") {
			t.Error("the follow-up tick's stop killed the runtime the attach is starting")
		}
	}
	mgr := session.NewManagerWithOptions(env.store, sp, session.WithClock(env.clk), session.WithCityPath(city))
	if err := mgr.Attach(context.Background(), b.ID, "test-cmd", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if sp.tick != nil {
		t.Fatal("the mid-resume ticks did not run (test premise)")
	}
	stops := env.sp.CountCalls("Stop", "worker")
	for i := 0; i < 6; i++ {
		env.clk.Time = env.clk.Time.Add(defaultDrainTimeout + time.Minute)
		env.userHoldTickAt(t, city, b.ID, dops)
		if !env.sp.IsRunning("worker") || env.sp.CountCalls("Stop", "worker") != stops {
			t.Fatalf("tick %d stopped the attached runtime; stdout:\n%s\nstderr:\n%s", i+1, env.stdout.String(), env.stderr.String())
		}
	}
	if info := env.sessionInfo(b.ID); info.SleepIntent != "" || info.HeldUntil != "" {
		t.Fatalf("row sleep_intent=%q held_until=%q, want the hold consumed", info.SleepIntent, info.HeldUntil)
	}
}

// A pass that read the row before the attach's consume reaches the drain-ack
// after it (the reviewer's stale-snapshot window), on the start and the live
// branch. Its snapshot still shows the user hold, so the snapshot guard passes
// the ack; the stop-pending marker is then decided on a fresh read under the
// runtime lease, finds the hold consumed, and drops the ack instead of
// marking the row. The queued stop, if any, runs to completion before the
// test checks the runtime.
func TestAttachSurvivesAStaleSnapshotDrainAck(t *testing.T) {
	for _, branch := range []string{"start", "live"} {
		t.Run(branch, func(t *testing.T) {
			env, b, dops := heldDrainedSession(t)
			city := t.TempDir()
			var before beads.Bead
			sp := &tickOnStart{Fake: env.sp}
			if branch == "live" {
				// The managed suspend's runtime is still up: legacy heals the row
				// awake and publishes the user-hold drain's ack.
				if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "test-cmd"}); err != nil {
					t.Fatal(err)
				}
				if err := env.store.SetMetadataBatch(b.ID, map[string]string{"state": "suspended", "state_reason": ""}); err != nil {
					t.Fatal(err)
				}
				env.userHoldTickAt(t, city, b.ID, dops)
				before, _ = env.store.Get(b.ID)
			} else {
				sp.tick = func() {
					env.userHoldTickAt(t, city, b.ID, dops)
					before, _ = env.store.Get(b.ID)
				}
			}
			mgr := session.NewManagerWithOptions(env.store, sp, session.WithClock(env.clk), session.WithCityPath(city))
			if err := mgr.Attach(context.Background(), b.ID, "test-cmd", runtime.Config{}); err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if acked, _ := dops.isDrainAcked("worker"); !acked || before.Metadata["sleep_intent"] != "user-hold" {
				t.Fatalf("acked=%v snapshot intent=%q, want an ack and a held snapshot (test premise)", acked, before.Metadata["sleep_intent"])
			}
			stops := &asyncStartTracker{}
			env.clk.Time = env.clk.Time.Add(time.Minute)
			env.userHoldTickOn(t, city, before, dops, withAsyncDrainAckStopTracker(stops))
			waitAsyncStopsForTest(t, stops)
			if !env.sp.IsRunning("worker") {
				t.Fatalf("the stale-snapshot drain-ack stopped the resumed runtime; stdout:\n%s", env.stdout.String())
			}
			if info := env.sessionInfo(b.ID); info.StateReason == session.DrainAckStopPendingReason || info.SleepIntent != "" {
				t.Fatalf("row state_reason=%q sleep_intent=%q, want no stop-pending marker over the consumed row", info.StateReason, info.SleepIntent)
			}
			for i := 0; i < 3; i++ {
				env.clk.Time = env.clk.Time.Add(defaultDrainTimeout + time.Minute)
				env.userHoldTickAt(t, city, b.ID, dops)
				if !env.sp.IsRunning("worker") {
					t.Fatalf("tick %d stopped the resumed runtime", i+1)
				}
			}
		})
	}
}
