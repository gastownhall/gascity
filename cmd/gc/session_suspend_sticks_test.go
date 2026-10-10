package main

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// mc-esqo7: an operator's suspend of a session holding started work sticks
// across legacy ticks. Every operator suspend writes the user hold
// (session.OperatorSuspendPatch), and a drain the suspend began completes
// without clearing sleep_intent=user-hold, so the far-future held_until never
// reads as a heartbeat hold that legacy's crash recovery restarts through.

// runningSeatWithWork is a pool seat holding in_progress work, woken by a
// real legacy tick, half an hour past its wake (out of the rapid-crash and
// churn windows).
func runningSeatWithWork(t *testing.T) (*killedSeatEnv, beads.Bead, string) {
	t.Helper()
	e := newKilledSeatEnv(t, persistentWorker())
	work := e.addWork(t, "in_progress", e.seat.ID, nil)
	e.tick(t, []beads.Bead{work}, withReadyAssignedFlags([]bool{true}))
	name := e.reload(t, e.seat.ID).Metadata["session_name"]
	if !e.sp.IsRunning(name) {
		t.Fatal("seat with ready in_progress work was not woken")
	}
	e.advance(30 * time.Minute)
	return e, work, name
}

func (e *killedSeatEnv) advance(d time.Duration) {
	e.clk.Time = e.clk.Time.Add(d)
	e.now = e.clk.Time
}

// readyTicks runs n legacy ticks a minute apart with the seat's work ready,
// the production wake-demand input, and returns how many Starts they made.
func (e *killedSeatEnv) readyTicks(t *testing.T, work beads.Bead, name string, n int) int {
	t.Helper()
	before := e.sp.CountCalls("Start", name)
	for i := 0; i < n; i++ {
		e.advance(time.Minute)
		e.tick(t, []beads.Bead{e.reload(t, work.ID)}, withReadyAssignedFlags([]bool{true}))
	}
	return e.sp.CountCalls("Start", name) - before
}

func TestOperatorSuspendSticksAcrossLegacyTicks(t *testing.T) {
	t.Run("API and CLI fallback (Manager.Suspend)", func(t *testing.T) {
		e, work, name := runningSeatWithWork(t)
		if err := session.NewManagerWithOptions(e.mem, e.sp, session.WithClock(e.clk), session.WithCityPath(e.city)).Suspend(e.seat.ID); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		if starts := e.readyTicks(t, work, name, 3); starts != 0 || e.sp.IsRunning(name) {
			t.Fatalf("suspended seat started %d times (running=%v), want it to stay down", starts, e.sp.IsRunning(name))
		}
		// Legacy's heal reads a suspended row with no runtime as asleep, as it
		// does a managed suspend's; the hold is what keeps it down.
		if m := e.reload(t, e.seat.ID).Metadata; m["state"] != string(session.StateAsleep) ||
			m["sleep_intent"] != string(session.SleepReasonUserHold) || m["held_until"] == "" {
			t.Fatalf("row state=%q sleep_intent=%q held_until=%q after ticks, want asleep with the user hold", m["state"], m["sleep_intent"], m["held_until"])
		}
	})

	t.Run("managed, drain completes by timeout", func(t *testing.T) {
		e, work, name := runningSeatWithWork(t)
		if err := sessionFrontDoor(e.mem).OperatorSuspend(e.seat.ID, e.clk.Now()); err != nil {
			t.Fatalf("OperatorSuspend: %v", err)
		}
		e.tick(t, []beads.Bead{work}, withReadyAssignedFlags([]bool{true})) // begins the user-hold drain
		e.advance(defaultDrainTimeout + time.Minute)
		e.tick(t, []beads.Bead{work}, withReadyAssignedFlags([]bool{true})) // stops it and completes the drain
		assertDrainedUserHold(t, e.reload(t, e.seat.ID), "user-hold")
		if starts := e.readyTicks(t, work, name, 3); starts != 0 || e.sp.IsRunning(name) {
			t.Fatalf("drained suspended seat started %d times (running=%v), want it held down", starts, e.sp.IsRunning(name))
		}
	})

	t.Run("managed, drain-ack completes with assigned work", func(t *testing.T) {
		e, work, name := runningSeatWithWork(t)
		if err := sessionFrontDoor(e.mem).OperatorSuspend(e.seat.ID, e.clk.Now()); err != nil {
			t.Fatalf("OperatorSuspend: %v", err)
		}
		// The agent acked the user-hold drain and its runtime stopped: the
		// stop-pending row finalizes as asleep (idle) because it holds work.
		if err := e.sp.Stop(name); err != nil {
			t.Fatal(err)
		}
		if err := e.mem.SetMetadataBatch(e.seat.ID, session.DrainAckStopPendingPatch(e.clk.Now())); err != nil {
			t.Fatal(err)
		}
		info, err := sessionFrontDoor(e.mem).Get(e.seat.ID)
		if err != nil {
			t.Fatal(err)
		}
		finalizeDrainAckStoppedSession(e.city, e.cfg, e.mem, nil, info, "", false, newFakeDrainOps(), e.dt, e.clk, events.Discard, io.Discard)
		assertDrainedUserHold(t, e.reload(t, e.seat.ID), string(session.SleepReasonIdle))
		if starts := e.readyTicks(t, work, name, 3); starts != 0 || e.sp.IsRunning(name) {
			t.Fatalf("drain-acked suspended seat started %d times (running=%v), want it held down", starts, e.sp.IsRunning(name))
		}
	})

	// The control: a `gc runtime heartbeat` hold (held_until, no intent) on a
	// seat whose runtime died with work still respawns through the hold.
	t.Run("heartbeat hold still respawns", func(t *testing.T) {
		e, work, name := runningSeatWithWork(t)
		if code := doRuntimeHeartbeat(e.mem, time.Hour, name, name, false, io.Discard, io.Discard); code != 0 {
			t.Fatalf("heartbeat exit %d", code)
		}
		if err := e.sp.Stop(name); err != nil {
			t.Fatal(err)
		}
		if starts := e.readyTicks(t, work, name, 1); starts != 1 || !e.sp.IsRunning(name) {
			t.Fatalf("heartbeat-held dead seat started %d times (running=%v), want one respawn", starts, e.sp.IsRunning(name))
		}
	})
}

// TestHeldPoolSeatKeepsItsSlotAndWork: a pool seat an operator suspended
// is never freeable. Its drain completes, and the seat stays open with its
// claim assigned past strandedRepairConfirmGrace: no drained close, no
// stranded repair. The production drain ops persist across the ticks.
func TestHeldPoolSeatKeepsItsSlotAndWork(t *testing.T) {
	for _, status := range []string{"open", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			e := newKilledSeatEnv(t, persistentWorker())
			e.dops = newDrainOps(e.sp)
			work := e.addWork(t, status, e.seat.ID, nil)
			e.tick(t, []beads.Bead{work}, withReadyAssignedFlags([]bool{true}))
			name := e.reload(t, e.seat.ID).Metadata["session_name"]
			if !e.sp.IsRunning(name) {
				t.Fatal("seat was not woken (test premise)")
			}
			e.advance(30 * time.Minute)
			if err := sessionFrontDoor(e.mem).OperatorSuspend(e.seat.ID, e.clk.Now()); err != nil {
				t.Fatal(err)
			}
			starts := e.sp.CountCalls("Start", name)
			for elapsed := time.Duration(0); elapsed < defaultDrainTimeout+strandedRepairConfirmGrace+5*time.Minute; elapsed += time.Minute {
				e.tick(t, []beads.Bead{e.reload(t, work.ID)})
				e.advance(time.Minute)
			}
			seat := e.reload(t, e.seat.ID)
			assertKeptOpen(t, seat)
			if seat.Metadata["sleep_intent"] != string(session.SleepReasonUserHold) || e.sp.IsRunning(name) || e.sp.CountCalls("Start", name) != starts {
				t.Fatalf("seat sleep_intent=%q running=%v starts=%d, want held down with no start", seat.Metadata["sleep_intent"], e.sp.IsRunning(name), e.sp.CountCalls("Start", name)-starts)
			}
			e.assertAssigned(t, work, status, e.seat.ID)
		})
	}
}

// TestDrainAckFinalizeKeepsAHeldSeatOpen: the drain-ack finalize closes an
// unassigned pool seat as drained, but never one an operator holds.
func TestDrainAckFinalizeKeepsAHeldSeatOpen(t *testing.T) {
	for _, held := range []bool{true, false} {
		env := newReconcilerTestEnv(t)
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
		b := env.createSessionBead("worker", "worker")
		front := sessionFrontDoor(env.store)
		if held {
			if err := front.OperatorSuspend(b.ID, env.clk.Now()); err != nil {
				t.Fatal(err)
			}
		}
		if err := front.ApplyPatch(b.ID, session.DrainAckStopPendingPatch(env.clk.Now())); err != nil {
			t.Fatal(err)
		}
		finalizeDrainAckStoppedSession(env.city, env.cfg, env.store, nil, env.sessionInfo(b.ID), "worker", true,
			newFakeDrainOps(), env.dt, env.clk, env.rec, &env.stderr)
		got, _ := env.store.Get(b.ID)
		if closed := got.Status == "closed"; closed == held {
			t.Fatalf("held=%v: closed=%v (close_reason=%q), want closed only when unheld", held, closed, got.Metadata["close_reason"])
		}
	}
}

// TestFreeableExcludesUserHold: both twins of the pool-slot freeable
// predicate refuse a row an operator holds, whatever its sleep reason says.
func TestFreeableExcludesUserHold(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, reason := range []string{"idle", "killed", ""} {
		for intent, want := range map[string]bool{"": true, "user-hold": false} {
			meta := map[string]string{"state": "asleep", "sleep_reason": reason, "slept_at": now.Add(-time.Hour).Format(time.RFC3339), "sleep_intent": intent}
			b := beads.Bead{ID: "s-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta}
			if got := isPoolSessionSlotFreeable(b, now); got != want {
				t.Errorf("bead reason %q intent %q: freeable = %v, want %v", reason, intent, got, want)
			}
			if got := isPoolSessionSlotFreeableInfo(session.Info{MetadataState: "asleep", SleepReason: reason, SleptAt: meta["slept_at"], SleepIntent: intent}, now); got != want {
				t.Errorf("info reason %q intent %q: freeable = %v, want %v", reason, intent, got, want)
			}
		}
	}
}

// assertDrainedUserHold: a completed suspend drain leaves the row asleep
// with the operator's intent and indefinite hold in place.
func assertDrainedUserHold(t *testing.T, row beads.Bead, sleepReason string) {
	t.Helper()
	m := row.Metadata
	if m["state"] != string(session.StateAsleep) || m["sleep_reason"] != sleepReason ||
		m["sleep_intent"] != string(session.SleepReasonUserHold) || m["held_until"] == "" {
		t.Fatalf("drained row state=%q sleep_reason=%q sleep_intent=%q held_until=%q, want asleep/%s with the user hold kept",
			m["state"], m["sleep_reason"], m["sleep_intent"], m["held_until"], sleepReason)
	}
}

// TestHeartbeatHeldExcludesOperatorSuspend is v2's SESS-602 predicate over
// the rows the production writers leave: only a heartbeat hold is one; a
// suspend is not, before or after its drain completes.
func TestHeartbeatHeldExcludesOperatorSuspend(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	suspended := session.Info{}.ApplyPatch(session.OperatorSuspendPatch(now))
	for name, tc := range map[string]struct {
		info session.Info
		want bool
	}{
		"heartbeat":                  {info: session.Info{HeldUntil: now.Add(time.Hour).Format(time.RFC3339)}, want: true},
		"suspended":                  {info: suspended},
		"suspend drained by timeout": {info: suspended.ApplyPatch(session.KeepUserHold(suspended, session.CompleteDrainPatch(now, "user-hold", false)))},
		"suspend drain-acked":        {info: suspended.ApplyPatch(session.KeepUserHold(suspended, session.CompleteDrainPatch(now, "idle", false)))},
	} {
		if got := heartbeatHeld(tc.info, now); got != tc.want {
			t.Errorf("%s: heartbeatHeld = %v, want %v", name, got, tc.want)
		}
	}
}

// TestWaitAndHeartbeatKeepAUserHold: a `gc session wait --sleep` and its
// clear, and a `gc runtime heartbeat`, on a row an operator suspended leave
// the operator's intent and held_until as they are, so none of them turns
// the user hold into a heartbeat hold. On an unheld row they act as before.
func TestWaitAndHeartbeatKeepAUserHold(t *testing.T) {
	for _, held := range []bool{true, false} {
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "worker", "template": "worker", "state": "active",
		}})
		if err != nil {
			t.Fatal(err)
		}
		front := sessionFrontDoor(store)
		if held {
			if err := front.OperatorSuspend(b.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		before, _ := store.Get(b.ID)
		if err := setSessionWaitHold(front, b.ID); err != nil {
			t.Fatal(err)
		}
		mid, _ := store.Get(b.ID)
		if err := clearSessionWaitHoldIfIdle(front, b.ID); err != nil {
			t.Fatal(err)
		}
		code := doRuntimeHeartbeat(store, time.Hour, "worker", "worker", false, io.Discard, io.Discard)
		after, _ := store.Get(b.ID)
		if held {
			if mid.Metadata["sleep_intent"] != "user-hold" || after.Metadata["sleep_intent"] != "user-hold" ||
				after.Metadata["held_until"] != before.Metadata["held_until"] || code == 0 {
				t.Fatalf("held: wait intent=%q, final intent=%q held_until %q -> %q, heartbeat exit %d; want the user hold untouched",
					mid.Metadata["sleep_intent"], after.Metadata["sleep_intent"], before.Metadata["held_until"], after.Metadata["held_until"], code)
			}
			continue
		}
		if mid.Metadata["sleep_intent"] != "wait-hold" || after.Metadata["sleep_intent"] != "" || after.Metadata["held_until"] == "" || code != 0 {
			t.Fatalf("unheld: wait intent=%q, final intent=%q held_until=%q, heartbeat exit %d", mid.Metadata["sleep_intent"], after.Metadata["sleep_intent"], after.Metadata["held_until"], code)
		}
	}
}

// TestSnapshotSleepsKeepAUserHoldThatLandedSince: the sleep writes decided
// from the tick's snapshot (the idle-timeout and max-age stops, the
// idle-stop-pending recovery, the city stop) read the row fresh, so an
// operator's suspend that landed after the snapshot keeps its intent in the
// store and in the tick's fold.
func TestSnapshotSleepsKeepAUserHoldThatLandedSince(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for name, sleep := range map[string]func(front *session.Store, snap session.Info) session.Info{
		"timer stop": func(front *session.Store, snap session.Info) session.Info {
			tick := newReconcileTick([]session.Info{snap})
			tick.applySleepOptimistic(snap.ID, front, session.SleepPatch(now, "idle-timeout"))
			return tick.infoByID[snap.ID]
		},
		"idle-stop-pending recovery": func(front *session.Store, snap session.Info) session.Info {
			snap.SleepIntent = "idle-stop-pending"
			return snap.ApplyPatch(recoverPendingIdleSleepInfo(snap, front, false, &clock.Fake{Time: now}))
		},
		"city stop": func(front *session.Store, snap session.Info) session.Info {
			markCityStopSessionAsAsleep(front, snap.ID, io.Discard)
			got, _ := front.Get(snap.ID)
			return got
		},
	} {
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "worker", "template": "worker", "state": "active",
		}})
		if err != nil {
			t.Fatal(err)
		}
		front := sessionFrontDoor(store)
		snap, err := front.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := front.OperatorSuspend(b.ID, now); err != nil {
			t.Fatal(err)
		}
		folded := sleep(front, snap)
		stored, _ := front.Get(b.ID)
		if stored.SleepIntent != "user-hold" || folded.SleepIntent != "user-hold" || stored.MetadataState != "asleep" {
			t.Errorf("%s: stored state=%q intent=%q, folded intent=%q; want asleep with the user hold kept", name, stored.MetadataState, stored.SleepIntent, folded.SleepIntent)
		}
	}
}

// TestIdleStopPendingClearKeepsAUserHold: the re-wake clear of a stale
// idle-stop-pending intent is decided on a fresh read, so a suspend that
// landed after the snapshot keeps its user-hold intent in the store and the
// fold; a row still idle-stop-pending is cleared as before.
func TestIdleStopPendingClearKeepsAUserHold(t *testing.T) {
	for suspended, want := range map[bool]string{false: "", true: "user-hold"} {
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "worker", "template": "worker", "state": "active", "sleep_intent": "idle-stop-pending",
		}})
		if err != nil {
			t.Fatal(err)
		}
		front := sessionFrontDoor(store)
		snap, _ := front.Get(b.ID)
		if suspended {
			if err := front.OperatorSuspend(b.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		tick := newReconcileTick([]session.Info{snap})
		tick.clearIdleStopPending(b.ID, front)
		stored, _ := front.Get(b.ID)
		if stored.SleepIntent != want || tick.infoByID[b.ID].SleepIntent != want {
			t.Errorf("suspended=%v: stored intent %q, folded %q, want %q", suspended, stored.SleepIntent, tick.infoByID[b.ID].SleepIntent, want)
		}
	}
}

// TestLegacyStartSkipsAUserHeldRow: the legacy start's locked re-read
// refuses a row an operator suspended after the tick's snapshot; nothing is
// written (no PreWake).
func TestLegacyStartSkipsAUserHeldRow(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "worker", "template": "worker", "state": "asleep",
	}})
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := sessionFrontDoor(store).Get(b.ID)
	if err := sessionFrontDoor(store).OperatorSuspend(b.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Get(b.ID)
	_, err = prepareStartCandidate(startCandidate{info: snap, tp: TemplateParams{Command: "true", SessionName: "worker", TemplateName: "worker"}},
		&config.City{}, store, &clock.Fake{Time: time.Now()})
	if !errors.Is(err, errStartUserHeld) {
		t.Fatalf("prepareStartCandidate = %v, want errStartUserHeld", err)
	}
	if after, _ := store.Get(b.ID); after.Metadata["state"] != before.Metadata["state"] || after.Metadata["generation"] != before.Metadata["generation"] {
		t.Fatalf("row changed: state %q -> %q, generation %q -> %q", before.Metadata["state"], after.Metadata["state"], before.Metadata["generation"], after.Metadata["generation"])
	}
}
