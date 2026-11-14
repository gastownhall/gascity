package main

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
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

// drainPool is the pool the seat sits in: whether a bead routed to its
// template waits for a seat (the ga-rxhu2 shape: every bead routed to the
// template unclaimable), and the template's max_active_sessions.
type drainPool struct {
	demand bool
	max    int
}

func (p drainPool) String() string {
	if !p.demand {
		return "no work routed"
	}
	return fmt.Sprintf("work routed, pool max %d", p.max)
}

// drainPools are the pools every fixture runs in: one nothing is routed to,
// and one with a routed bead at the smallest pool and at a pool with room.
var drainPools = []drainPool{{demand: false, max: 3}, {demand: true, max: 1}, {demand: true, max: 3}}

// drainRun is one fixture: shape's seat with its drain begun age before the
// run, in pool, on a runtime that answers a stop as rt says.
type drainRun struct {
	shape drainDeadlineShape
	age   time.Duration
	rt    drainStopMode
	pool  drainPool
	arms  func([]rowArm) []rowArm
}

// drainOutcome is what a run left behind.
type drainOutcome struct {
	s      *sim
	stops  *atomic.Int64 // refused stops
	events *events.Fake
	// freedAfter is how long into the run the seat was first freed (-1: never).
	freedAfter time.Duration
}

// drainBound is how long after the run starts a killable seat may take to
// free. With nothing routed the ordinary chain frees it far inside legacy's
// deadline; with work routed the pool counts the seat as the one it wants, so
// the ordinary chain may have nothing to finish and the bound is the deadline
// itself (counted from drain_at), plus a few passes to signal, stop and close.
func drainBound(age time.Duration, pool drainPool) time.Duration {
	if !pool.demand {
		return drainFreedWithin
	}
	return max(0, poolSlotDrainRetireDeadline-age) + 5*time.Minute
}

// runDrainDeadline seeds the run's seat, runs the sim for the horizon, and
// logs the pass-by-pass seat table: one line per change of the store's session
// rows, the runtimes, the starts, the kills or the stop alerts.
func runDrainDeadline(t *testing.T, d drainRun) drainOutcome {
	t.Helper()
	out := drainOutcome{stops: new(atomic.Int64), events: events.NewFake(), freedAfter: -1}
	o := simOpts{arms: d.arms}
	if d.rt != runtimeKillable {
		o.leaf = func(sp *simProvider) runtime.Provider {
			return simUnkillable{simProvider: sp, stops: out.stops, silent: d.rt == runtimeStopIgnored}
		}
	}
	rows := []beads.Bead{d.shape.row(d.age)}
	if d.pool.demand {
		rows = append(rows, beads.Bead{ID: "gw-1", Title: "work", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	}
	out.s = scriptedWith(t, o, []string{"gc-1"}, rows...)
	out.s.cfg.Agents[0].MaxActiveSessions = intPtr(d.pool.max)
	out.s.p.rec = out.events
	// The pane has been quiet for an hour: D6's quiet hold (a busy pane, or
	// activity the pass cannot read) must not mute the alert under test.
	out.s.sp.mu.Lock()
	out.s.sp.SetActivity("s-gc-1", out.s.clk.Now().Add(-time.Hour))
	out.s.sp.mu.Unlock()
	last := ""
	for elapsed := time.Duration(0); elapsed <= drainDeadlineHorizon; elapsed += 30 * time.Second {
		out.s.settle()
		if out.freedAfter < 0 && seatFreed(out.s.seatRow(t), out.s.clk.Now()) {
			out.freedAfter = elapsed
		}
		if line := out.s.seatTable(); line != last {
			t.Logf("+%-6s %s", elapsed, line)
			last = line
		}
		out.s.advance(30 * time.Second)
	}
	return out
}

// seatTable is one line of the seat table: every session row the store holds
// (id, status, state, sleep_reason, drain_finalize), the runtimes, and the
// counts of starts, kills and D6's stop alerts.
func (s *sim) seatTable() string {
	rows, _ := s.legs[0].backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	var seats []string
	for _, b := range rows {
		if b.Metadata["session_name"] == "" {
			continue
		}
		seat := fmt.Sprintf("%s:%s/%s", b.ID, b.Status, b.Metadata["state"])
		for _, k := range []string{"sleep_reason", drainFinalizeMetadataKey} {
			if v := b.Metadata[k]; v != "" {
				seat += "/" + k + "=" + v
			}
		}
		seats = append(seats, seat)
	}
	slices.Sort(seats)
	s.sp.mu.Lock()
	var rts []string
	for name := range s.sp.rts {
		rts = append(rts, name)
	}
	starts, kills := len(s.sp.starts), len(s.sp.kills)
	s.sp.mu.Unlock()
	slices.Sort(rts)
	return fmt.Sprintf("rows %v runtimes %v starts %d kills %d stop_escalations %d", seats, rts, starts, kills, s.p.metrics.series["stop_escalations"])
}

// startedUnderNewName reports a runtime started for the routed bead under a
// session name other than the stuck seat's, and still running.
func (s *sim) startedUnderNewName() (string, bool) {
	s.sp.mu.Lock()
	defer s.sp.mu.Unlock()
	for _, st := range s.sp.starts {
		if _, alive := s.sp.rts[st.Name]; alive && st.Name != "" && st.Name != "s-gc-1" {
			return st.Name, true
		}
	}
	return "", false
}

// retirements counts the events the pool-slot drain deadline records.
func (o drainOutcome) retirements() int {
	n := 0
	for _, e := range o.events.Events {
		if e.Type == events.SessionPoolSlotRetiredAtDrainDeadline {
			n++
		}
	}
	return n
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
// runtime for longer than legacy's bound allows, in a pool nothing is routed
// to and in one with a bead routed to its template (the ga-rxhu2 shape: the
// pool counts the stuck seat as the seat it wants, so the ordinary drain has
// nothing to finish).
//
// Every run frees the seat within drainBound, and with work routed the pool
// starts a seat for the bead under a new session name. A seat the bound
// retires carries drain_finalize=deadline, one close and one retirement event,
// and never before the deadline: a drain that finalizes inside it (begun a
// minute short of it) finishes through the ordinary chain, with no stamp.
func TestSimPoolSeatInDrainIsFreedWithAKillableRuntime(t *testing.T) {
	for _, shape := range drainDeadlineShapes() {
		for _, pool := range drainPools {
			for _, age := range []time.Duration{time.Minute, poolSlotDrainRetireDeadline - time.Minute, 2 * poolSlotDrainRetireDeadline} {
				t.Run(fmt.Sprintf("%s, %s, drain begun %s before", shape.name, pool, age), func(t *testing.T) {
					out := runDrainDeadline(t, drainRun{shape: shape, age: age, rt: runtimeKillable, pool: pool})
					s, b := out.s, out.s.seatRow(t)
					bound := drainBound(age, pool)
					if out.freedAfter < 0 || out.freedAfter > bound {
						t.Errorf("seat freed after %v (negative: never), now %s %v; want within %v", out.freedAfter, b.Status, b.Metadata, bound)
					}
					stamp, retired := b.Metadata[drainFinalizeMetadataKey], out.retirements()
					switch {
					case stamp == "" && retired != 0:
						t.Errorf("%d retirement events for a seat with no %s", retired, drainFinalizeMetadataKey)
					case stamp == "":
					case !pool.demand:
						t.Errorf("%s = %q: the ordinary chain finalizes a drain nothing is routed to, so the bound has no seat to retire", drainFinalizeMetadataKey, stamp)
					case stamp != drainFinalizeDeadline:
						t.Errorf("%s = %q, want %q", drainFinalizeMetadataKey, stamp, drainFinalizeDeadline)
					case b.Status != "closed" || retired != 1:
						t.Errorf("stamped %s=%s on a %s seat with %d retirement events; want one close and one event", drainFinalizeMetadataKey, stamp, b.Status, retired)
					case out.freedAfter < poolSlotDrainRetireDeadline-age:
						t.Errorf("retired at +%v, before the deadline at +%v", out.freedAfter, poolSlotDrainRetireDeadline-age)
					}
					if pool.demand {
						if name, ok := s.startedUnderNewName(); !ok {
							t.Errorf("no seat was started for the routed bead (starts %+v): the pool is short of seats", s.sp.starts)
						} else {
							t.Logf("the routed bead's seat runs as %s", name)
						}
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
}

// Kills a v2 that closes a pool seat over a runtime it could not stop, and
// one that never says so (PAR-FORCEKILL is accepted: v2 has no force path,
// so the bead stays open and D6 alerts): every shape, in every pool, with a
// runtime whose Stop fails, keeps its row open, unstamped, and its runtime in
// place for the whole horizon, and a signaled stop raises
// session.drain_stop_escalated (stop_escalations) once it is 30 minutes old.
func TestSimPoolSeatInDrainStaysOpenOverAnUnkillableRuntime(t *testing.T) {
	for _, shape := range drainDeadlineShapes() {
		for _, pool := range drainPools {
			for _, rt := range []drainStopMode{runtimeStopRefused, runtimeStopIgnored} {
				t.Run(fmt.Sprintf("%s, %s, %s", shape.name, pool, rt), func(t *testing.T) {
					out := runDrainDeadline(t, drainRun{shape: shape, age: 2 * poolSlotDrainRetireDeadline, rt: rt, pool: pool})
					s, b := out.s, out.s.seatRow(t)
					if out.freedAfter >= 0 {
						t.Errorf("seat freed after %v (%s %v) over a runtime that never stopped: its slot is open while the agent runs", out.freedAfter, b.Status, b.Metadata)
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
					if n := out.retirements(); n != 0 {
						t.Errorf("%d retirement events over a runtime that never stopped", n)
					}
					// Each shape ends in a signaled stop (legacy's stop-pending
					// projection) that the runtime refuses: the stop is tried, and
					// D6 alerts once it is 30 minutes old.
					if out.stops.Load() == 0 {
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
}

// Kills a fixture that cannot see a wedge: with the arms that finalize a
// drain (A4's stop, A11's signal, A20's begin, A21's close) and the bound's
// own removed from the registry, the same seats stay parked for the whole
// horizon, as the ga-rxhu2 pool did. The two tests above pass only because
// v2's chain frees a seat the control leaves stuck, so a green there is
// evidence, not an artifact of the harness.
func TestSimPoolSeatInDrainControlWithoutTheDrainArmsStaysStuck(t *testing.T) {
	without := func(drop ...string) func([]rowArm) []rowArm {
		return func(arms []rowArm) []rowArm {
			return slices.DeleteFunc(arms, func(a rowArm) bool { return slices.Contains(drop, a.name) })
		}
	}
	for _, shape := range drainDeadlineShapes() {
		for _, pool := range drainPools[1:] {
			t.Run(shape.name+", "+pool.String(), func(t *testing.T) {
				out := runDrainDeadline(t, drainRun{shape: shape, age: 2 * poolSlotDrainRetireDeadline, rt: runtimeKillable, pool: pool, arms: without("A4", "A11", "A20", "A21", "A22")})
				if out.freedAfter >= 0 {
					t.Errorf("seat freed after %v with the drain arms removed: the fixture does not depend on them, so it proves nothing", out.freedAfter)
				}
			})
		}
	}
}
