package main

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// PAR-DRAINDL on the simulator: does v2's own drain chain (A6 heals, A20
// begin, A11 ack, A4 stop, A21 close) bound how long a pool seat sits in an
// unfinalized drain, as legacy's retirePoolSlotAtDrainDeadline does at
// poolSlotDrainRetireDeadline after drain_at (ga-rxhu2: a pool at ZERO seats
// for 3d10h, every bead routed to the template unclaimable)?
//
// The answer is that it does, with no arm of its own: a seat in any drainDeadlineShapes
// shape is walked to a close by armDrainBegin (begin), armSignal (the stop
// request, a grace after the begin), armAck (the agent's ack), armStop (the
// fenced stop: stopFenced, confirmStopped, then stopFinalizeStep's goneNow
// re-check) and armClose, and a runtime that will not stop keeps its row open
// with escalateStop's alert (PAR-FORCEKILL accepts that v2 has no force path).
// Legacy's drain_finalize=deadline write has no v2 counterpart and none is needed.
//
// What the fixture models: one pool seat of the "worker" template that no
// work is routed to, parked in each of the shapes a drain can be left in
// (drainDeadlineShapes), with its drain begun well before the run (so the
// legacy bound would already be due) or just now, on a runtime that stops
// when asked (killable) or one whose Stop fails and which never leaves the
// name (unkillable), run on the fake clock for several multiples of the
// deadline.
//
// What it does not model: a real agent process (the runtime acks nothing
// unless the row says it did; "attached" is never set, so D6's quiet hold
// does not apply), a controller restart (the planner's memory is fresh
// every run), a second seat or a pool that wants the seat (a seat with work
// is not in a drain the bound would retire), and tmux itself (a runtime that
// "survives" is the sim's, not a real pane).

// drainDeadlineHorizon is how long each fixture runs: four of legacy's
// deadlines, so a bound that only fires at the deadline has fired twice over.
const drainDeadlineHorizon = 4 * poolSlotDrainRetireDeadline

// drainFreedWithin is how soon after the run starts a killable seat must be
// freed by the ordinary chain: far inside legacy's deadline, which its own
// header requires to stay well above the ordinary path.
const drainFreedWithin = poolSlotDrainRetireDeadline / 3

// drainDeadlineShape is one way a pool seat is left in a drain.
type drainDeadlineShape struct {
	name string
	// row is the seat gc-1 as the drain left it, drainAge after drain_at.
	row func(drainAge time.Duration) beads.Bead
}

func drainDeadlineShapes() []drainDeadlineShape {
	meta := []string{"instance_token", "tok-1", "session_key", "k-1"}
	return []drainDeadlineShape{
		{
			// The ga-rxhu2 shape (session_pool_drain_deadline.go's header):
			// the drain-ack path stamped drain_at and sleep_reason=drained
			// and cleared last_woke_at, the runtime survived, and the state
			// heal read the row back to awake over the live runtime.
			name: "healed awake after a drain, runtime alive",
			row: func(age time.Duration) beads.Bead {
				return poolRow("gc-1", "worker", 1, "awake", append(slices.Clone(meta), "sleep_reason", "drained", "drain_at", rowAt(-age))...)
			},
		},
		{
			name: "drain-ack stop pending",
			row: func(age time.Duration) beads.Bead {
				return poolRow("gc-1", "worker", 1, "draining", append(slices.Clone(meta),
					"state_reason", session.DrainAckStopPendingReason, "sleep_reason", "drained", "drain_at", rowAt(-age))...)
			},
		},
		{
			name: "drain requested, never signaled",
			row: func(age time.Duration) beads.Bead {
				return poolRow("gc-1", "worker", 1, "active", append(slices.Clone(meta),
					session.DrainIntentReasonKey, reasonNoWake, session.DrainIntentAtKey, rowAt(-age), session.DrainIntentIncarnationKey, "1",
					"drain_at", rowAt(-age))...)
			},
		},
		{
			name: "drain acked by the agent, stop never taken",
			row: func(age time.Duration) beads.Bead {
				return poolRow("gc-1", "worker", 1, "active", append(slices.Clone(meta),
					session.OperatorDrainKey, "1",
					session.DrainAckIncarnationKey, "1", session.DrainAckAtKey, rowAt(-age), "drain_at", rowAt(-age))...)
			},
		},
	}
}

// simUnkillable is a provider whose Stop leaves the runtime in place: the
// agent that will not die (ga-rxhu2's unkillable half). Stop returns an error,
// or, when silent, nil: a kill that was accepted and did nothing, which only
// the fresh read after it can catch.
type simUnkillable struct {
	*simProvider
	stops  *atomic.Int64
	silent bool
}

var errSimUnkillable = errors.New("sim: the runtime refuses to stop")

func (u simUnkillable) Stop(string) error {
	u.stops.Add(1)
	if u.silent {
		return nil
	}
	return errSimUnkillable
}

// drainStopMode is how the seat's runtime answers a stop.
type drainStopMode int

const (
	runtimeKillable    drainStopMode = iota // Stop removes it
	runtimeStopRefused                      // Stop fails, it stays
	runtimeStopIgnored                      // Stop returns nil, it stays
)

func (d drainStopMode) String() string {
	return [...]string{"killable", "stop refused", "stop ignored"}[d]
}

// runDrainDeadline seeds shape's seat with its drain begun drainAge before the
// run, on a killable or unkillable runtime, runs the sim for the horizon and
// returns it with the count of refused stops and how long into the run the
// seat was first freed (-1: never).
func runDrainDeadline(t *testing.T, shape drainDeadlineShape, drainAge time.Duration, rt drainStopMode, arms func([]rowArm) []rowArm) (s *sim, stops *atomic.Int64, freedAfter time.Duration) {
	t.Helper()
	stops = new(atomic.Int64)
	o := simOpts{arms: arms}
	if rt != runtimeKillable {
		o.leaf = func(sp *simProvider) runtime.Provider {
			return simUnkillable{simProvider: sp, stops: stops, silent: rt == runtimeStopIgnored}
		}
	}
	s = scriptedWith(t, o, []string{"gc-1"}, shape.row(drainAge))
	// The pane has been quiet for an hour: D6's quiet hold (a busy pane, or
	// activity the pass cannot read) must not mute the alert under test.
	s.sp.mu.Lock()
	s.sp.SetActivity("s-gc-1", s.clk.Now().Add(-time.Hour))
	s.sp.mu.Unlock()
	freedAfter = -1
	for elapsed := time.Duration(0); elapsed <= drainDeadlineHorizon; elapsed += 30 * time.Second {
		s.settle()
		if freedAfter < 0 && seatFreed(s.seatRow(t), s.clk.Now()) {
			freedAfter = elapsed
		}
		s.advance(30 * time.Second)
	}
	return s, stops, freedAfter
}

// seatRow is the seat gc-1 as the store holds it.
func (s *sim) seatRow(t *testing.T) beads.Bead {
	t.Helper()
	b, err := s.legs[0].backing.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seatFreed reports a seat that no longer holds its pool slot: closed, or
// asleep with the slot freeable.
func seatFreed(b beads.Bead, now time.Time) bool {
	if b.Status == "closed" {
		return true
	}
	info := session.Info{MetadataState: b.Metadata["state"], SleepReason: b.Metadata["sleep_reason"]}
	return b.Metadata["state"] == "asleep" && isPoolSessionSlotFreeableInfo(info, now)
}

// Kills a v2 that leaves a pool seat in an unfinalized drain with a killable
// runtime for longer than legacy's bound allows: every shape, begun
// before the run or at it, must free its seat well inside the deadline, by
// the ordinary chain (no drain_finalize=deadline, which only legacy's bound
// writes), starting nothing under its name.
func TestSimPoolSeatInDrainIsFreedWithAKillableRuntime(t *testing.T) {
	for _, shape := range drainDeadlineShapes() {
		for _, age := range []time.Duration{time.Minute, 2 * poolSlotDrainRetireDeadline} {
			t.Run(fmt.Sprintf("%s, drain begun %s before", shape.name, age), func(t *testing.T) {
				s, _, freedAfter := runDrainDeadline(t, shape, age, runtimeKillable, nil)
				b := s.seatRow(t)
				if freedAfter < 0 || freedAfter > drainFreedWithin {
					t.Errorf("seat freed after %v (-1: never), now %s %v; want within %v, well inside legacy's %s bound", freedAfter, b.Status, b.Metadata, drainFreedWithin, poolSlotDrainRetireDeadline)
				}
				if got := b.Metadata[drainFinalizeMetadataKey]; got != "" {
					t.Errorf("%s = %q; only legacy's bound writes it", drainFinalizeMetadataKey, got)
				}
				for _, st := range s.sp.starts {
					if st.Name == "s-gc-1" {
						t.Errorf("start %+v: nothing may start under a draining seat's name", st)
					}
				}
				s.noViolations(t)
			})
		}
	}
}

// Kills a v2 that closes a pool seat over a runtime it could not stop, and
// one that never says so (PAR-FORCEKILL is accepted: v2 has no force path,
// so the bead stays open and D6 alerts): every shape, with a runtime whose
// Stop fails, keeps its row open and its runtime in place for the whole
// horizon, and a signaled stop raises session.drain_stop_escalated
// (stop_escalations) once it is 30 minutes old.
func TestSimPoolSeatInDrainStaysOpenOverAnUnkillableRuntime(t *testing.T) {
	for _, shape := range drainDeadlineShapes() {
		for _, rt := range []drainStopMode{runtimeStopRefused, runtimeStopIgnored} {
			t.Run(shape.name+", "+rt.String(), func(t *testing.T) {
				s, stops, freedAfter := runDrainDeadline(t, shape, 2*poolSlotDrainRetireDeadline, rt, nil)
				b := s.seatRow(t)
				if freedAfter >= 0 {
					t.Errorf("seat freed after %v (%s %v) over a runtime that never stopped: its slot is open while the agent runs", freedAfter, b.Status, b.Metadata)
				}
				s.sp.mu.Lock()
				_, alive := s.sp.rts["s-gc-1"]
				kills := len(s.sp.kills)
				s.sp.mu.Unlock()
				if !alive {
					t.Errorf("the unkillable runtime left its name; the fixture is not modeling it")
				}
				if kills != 0 {
					t.Errorf("kills %+v; the fixture's runtime cannot be killed", s.sp.kills)
				}
				if got := b.Metadata[drainFinalizeMetadataKey]; got != "" {
					t.Errorf("%s = %q over a runtime that never stopped", drainFinalizeMetadataKey, got)
				}
				// Each shape ends in a signaled stop (legacy's stop-pending
				// projection) that the runtime refuses: the stop is tried, and
				// D6 alerts once it is 30 minutes old.
				if stops.Load() == 0 {
					t.Errorf("no stop was ever attempted")
				}
				if got := s.p.metrics.series["stop_escalations"]; got == 0 {
					t.Errorf("stop_escalations = 0 after %s with the stop outstanding; want D6's alert", drainDeadlineHorizon)
				}
				s.noViolations(t)
			})
		}
	}
}

// Kills a fixture that cannot see a wedge: with the arms that finalize a
// drain (A4's stop, A11's signal, A20's begin, A21's close) removed from the
// registry, the same seats stay parked for the whole horizon, as the
// ga-rxhu2 pool did. The two tests above pass only because v2's chain frees
// a seat the control leaves stuck, so a green there is evidence, not an
// artifact of the harness.
func TestSimPoolSeatInDrainControlWithoutTheDrainArmsStaysStuck(t *testing.T) {
	without := func(drop ...string) func([]rowArm) []rowArm {
		return func(arms []rowArm) []rowArm {
			return slices.DeleteFunc(arms, func(a rowArm) bool { return slices.Contains(drop, a.name) })
		}
	}
	for _, shape := range drainDeadlineShapes() {
		t.Run(shape.name, func(t *testing.T) {
			_, _, freedAfter := runDrainDeadline(t, shape, 2*poolSlotDrainRetireDeadline, runtimeKillable, without("A4", "A11", "A20", "A21"))
			if freedAfter >= 0 {
				t.Errorf("seat freed after %v with the drain arms removed: the fixture does not depend on them, so it proves nothing", freedAfter)
			}
		})
	}
}
