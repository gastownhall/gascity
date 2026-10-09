package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
		if err := session.NewManagerWithOptions(e.mem, e.sp, session.WithClock(e.clk)).Suspend(e.seat.ID); err != nil {
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
		if err := sessionFrontDoor(e.mem).OperatorSuspend(e.seat.ID, e.clk.Now(), nil); err != nil {
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
		if err := sessionFrontDoor(e.mem).OperatorSuspend(e.seat.ID, e.clk.Now(), nil); err != nil {
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
		"suspend drained by timeout": {info: suspended.ApplyPatch(session.CompleteDrainPatch(suspended, now, "user-hold"))},
		"suspend drain-acked":        {info: suspended.ApplyPatch(session.CompleteDrainPatch(suspended, now, "idle"))},
	} {
		if got := heartbeatHeld(tc.info, now); got != tc.want {
			t.Errorf("%s: heartbeatHeld = %v, want %v", name, got, tc.want)
		}
	}
}
