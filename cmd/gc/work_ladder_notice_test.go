package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workladder"
)

// W5: the work ladder's nudges as conversation-lane notices.

// ladderLaneCity is a ladderCity whose session rows a lane can serve: the
// sessions store mints gc- ids, and a lane host over it may be registered.
type ladderLaneCity struct {
	*ladderCity
	f      *laneFixture // the seat as a lane sees it
	host   *conversationLaneHost
	cancel context.CancelFunc
}

// ladderClock reads a ladder city's clock, so the host stamps its ops with
// the ladder's time.
type ladderClock struct{ c *ladderCity }

func (k ladderClock) Now() time.Time { return k.c.now }

// newLadderLaneCity is newLadderCity over a lane-capable sessions store,
// with a running lane host latched to kinds (none registered for no kinds).
func newLadderLaneCity(t *testing.T, kinds ...string) *ladderLaneCity {
	t.Helper()
	return newLadderLaneCityOver(t, func(s laneTestStore) beads.Store { return s }, kinds...)
}

// newLadderLaneCityOver is newLadderLaneCity with the host's and the
// ladder's sessions store wrapped by wrap.
func newLadderLaneCityOver(t *testing.T, wrap func(laneTestStore) beads.Store, kinds ...string) *ladderLaneCity {
	t.Helper()
	inner := newLaneTestStore()
	c := newLadderCityOver(t, inner.MemStore)
	store := wrap(inner)
	c.env.CityStore = blindWriteRefusingStore{inner: store}
	lc := &ladderLaneCity{ladderCity: c, f: &laneFixture{
		dir: c.env.CityPath, cfg: c.env.Cfg, store: store, sp: c.sp, clk: &laneTestClock{now: c.now},
		id: c.seat, name: ladderSeatName, token: "tok-7", started: true,
	}}
	if len(kinds) > 0 {
		lc.register(kinds...)
	}
	return lc
}

// register runs a lane host over the city's sessions store with kinds
// latched and no context, so Poke starts no pass.
func (lc *ladderLaneCity) register(kinds ...string) {
	lc.t.Helper()
	h := newConversationLaneHost(&laneHostEnvFake{f: lc.f, store: lc.f.store}, kinds, session.StopRequestsLive)
	h.clk = ladderClock{lc.ladderCity}
	h.state = laneHostStateRunning
	ctx, cancel := context.WithCancel(context.Background())
	lc.t.Cleanup(cancel)
	registerLaneHost(ctx, lc.f.dir, h)
	lc.host, lc.cancel = h, cancel
}

// stopHost turns the host off: it no longer executes message.
func (lc *ladderLaneCity) stopHost() {
	lc.host.mu.Lock()
	lc.host.state = laneHostStateOff
	lc.host.mu.Unlock()
}

func (lc *ladderLaneCity) st() *opjournal.Store { return lc.f.laneStore(lc.t) }

// ladderOps are the seat's open ladder notices.
func (lc *ladderLaneCity) ladderOps() []opjournal.Op {
	lc.t.Helper()
	var out []opjournal.Op
	for _, op := range lc.f.openOps(lc.t) {
		if op.Producer.Principal == ladderNoticePrincipal {
			out = append(out, op)
		}
	}
	return out
}

// onlyOp is the seat's one open ladder notice.
func (lc *ladderLaneCity) onlyOp() opjournal.Op {
	lc.t.Helper()
	ops := lc.ladderOps()
	if len(ops) != 1 {
		lc.t.Fatalf("open ladder notices = %+v, want one", ops)
	}
	return ops[0]
}

func (lc *ladderLaneCity) op(id string) opjournal.Op {
	lc.t.Helper()
	return lc.f.get(lc.t, lc.st(), id)
}

func (lc *ladderLaneCity) closeOp(id string, o opjournal.Outcome) {
	lc.t.Helper()
	if _, err := lc.st().Close(id, o, "lane:ctl"); err != nil {
		lc.t.Fatalf("closing %s: %v", id, err)
	}
}

func (lc *ladderLaneCity) startOp(id string) {
	lc.t.Helper()
	if _, err := lc.st().Transition(id, opjournal.StatusSubmitted, opjournal.StatusExecuting, "lane:ctl"); err != nil {
		lc.t.Fatalf("starting %s: %v", id, err)
	}
}

// at passes the step once at d after the start.
func (lc *ladderLaneCity) at(d time.Duration) {
	lc.now = lc.start.Add(d)
	lc.pass()
}

func (lc *ladderLaneCity) nudges() int { return lc.sp.CountCalls("Nudge", ladderSeatName) }

// startedClaim is a claim the hook's stamp names started.
func (lc *ladderLaneCity) startedClaim() string {
	id := lc.claim("the step")
	lc.setRow(map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: id})
	return id
}

func failed(reason string) opjournal.Outcome {
	return opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: reason}
}

var succeeded = opjournal.Outcome{Arm: opjournal.ArmSucceeded}

// wantLadderKey is the pinned key of e's notice at ordinal n:
// work-ladder:<token>:<bead>[/<store>]:<episode>:<ordinal>.
func wantLadderKey(token string, e workladder.Entry, n int) string {
	trigger := e.Key.Bead
	if e.Key.Store != "" {
		trigger += "/" + e.Key.Store
	}
	return fmt.Sprintf("work-ladder:%s:%s:%d:%d", token, trigger, e.Since.UnixNano(), n)
}

func TestWorkLadderNudgeIsALaneNotice(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	if n := lc.nudges(); n != 0 {
		t.Fatalf("typed %d nudges, want the notice only", n)
	}
	op := lc.onlyOp()
	e := lc.entry(id)
	m := op.Policy.Message
	switch {
	case op.Kind != opjournal.KindMessage || op.Executor != opjournal.ExecutorLane:
		t.Fatalf("op kind %s executor %s", op.Kind, op.Executor)
	case op.Producer != (opjournal.Producer{Principal: "gc:planner/work-ladder", Actor: opjournal.ActorController}):
		t.Fatalf("producer %+v", op.Producer)
	case m.Role != opjournal.RoleNotice || m.When != opjournal.WhenBoundary || m.Wake != opjournal.WakeLiveOnly:
		t.Fatalf("policy %+v", m)
	case op.Scope != (opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 7}):
		t.Fatalf("scope %+v, want session(7)", op.Scope)
	case op.Causation != id:
		t.Fatalf("causation %q, want %s", op.Causation, id)
	case op.ClientKey != wantLadderKey("tok-7", e, 1):
		t.Fatalf("key %q, want %q", op.ClientKey, wantLadderKey("tok-7", e, 1))
	case !op.StartBy.Equal(lc.now.Add(2*time.Minute + 45*time.Second)):
		t.Fatalf("start_by %v, want +2m45s", op.StartBy.Sub(lc.now))
	case op.Payload != fmt.Sprintf(ladderStartedNudge, id):
		t.Fatalf("payload %q", op.Payload)
	}
	if e.Notice != (workladder.NoticeMark{Op: op.ID, At: lc.now}) || e.Attempts != 1 {
		t.Fatalf("entry %+v, want attempt 1 marked with %s", e, op.ID)
	}
	if n := len(lc.events(events.WorkNudged)); n != 0 {
		t.Fatalf("work.nudged = %d before the notice was delivered", n)
	}
}

func TestWorkLadderNudgesDirectlyWithoutAMessageLane(t *testing.T) {
	for name, setup := range map[string]func(*ladderLaneCity){
		"no host":                 func(*ladderLaneCity) {},
		"a host latched to other": func(lc *ladderLaneCity) { lc.register("interrupt") },
		"a host not running": func(lc *ladderLaneCity) {
			lc.register("message")
			lc.stopHost()
		},
	} {
		t.Run(name, func(t *testing.T) {
			lc := newLadderLaneCity(t)
			setup(lc)
			id := lc.startedClaim()
			lc.at(0)
			lc.at(halfPast(1))
			if n := lc.nudges(); n != 1 {
				t.Fatalf("typed %d nudges, want the one direct nudge", n)
			}
			if ops := lc.ladderOps(); len(ops) != 0 {
				t.Fatalf("ops %+v, want none", ops)
			}
			if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 1 {
				t.Fatalf("entry %+v, want attempt 1, unmarked", e)
			}
			if n := len(lc.events(events.WorkNudged)); n != 1 {
				t.Fatalf("work.nudged = %d, want 1", n)
			}
		})
	}
}

func TestWorkLadderNudgesAProtocolModeSeatDirectly(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	lc.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	if n, ops := lc.nudges(), lc.ladderOps(); n != 1 || len(ops) != 0 {
		t.Fatalf("protocol mode: %d nudges, ops %+v; want the direct nudge", n, ops)
	}

	g := newLadderLaneCity(t, "message")
	g.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	stampHost(t, g.f, g.st(), 7, opjournal.KindMessage)
	g.startedClaim()
	g.at(0)
	g.at(halfPast(1))
	if n := g.nudges(); n != 0 {
		t.Fatalf("hosted elsewhere: %d direct nudges", n)
	}
	g.onlyOp()
}

func TestWorkLadderSettlesItsNoticeFromItsOutcome(t *testing.T) {
	type want struct {
		marked                    bool
		attempts, paused, refused int
		nudged, failed            int
	}
	for _, tc := range []struct {
		name  string
		after time.Duration // when the settling pass runs, from the proposal
		do    func(lc *ladderLaneCity, op opjournal.Op)
		want  want
	}{
		{"succeeded", 30 * time.Second, func(lc *ladderLaneCity, op opjournal.Op) { lc.closeOp(op.ID, succeeded) }, want{false, 1, 0, 0, 1, 0}},
		{"failed deadline_in_flight", 30 * time.Second, func(lc *ladderLaneCity, op opjournal.Op) {
			lc.closeOp(op.ID, failed(opjournal.ReasonDeadlineInFlight))
		}, want{false, 1, 0, 0, 0, 1}},
		{"failed poison", 30 * time.Second, func(lc *ladderLaneCity, op opjournal.Op) { lc.closeOp(op.ID, failed(opjournal.ReasonPoison)) }, want{false, 1, 0, 0, 0, 1}},
		{"failed expired_before_start", 30 * time.Second, func(lc *ladderLaneCity, op opjournal.Op) {
			lc.closeOp(op.ID, failed(opjournal.ReasonExpiredBeforeStart))
		}, want{false, 0, 1, 1, 0, 1}},
		{"failed provider_unavailable", 30 * time.Second, func(lc *ladderLaneCity, op opjournal.Op) {
			lc.closeOp(op.ID, failed(opjournal.ReasonProviderUnavailable))
		}, want{false, 0, 1, 1, 0, 1}},
		{"canceled", 30 * time.Second, func(lc *ladderLaneCity, op opjournal.Op) {
			if _, _, err := lc.st().Cancel(op.ID, "operator", "not now"); err != nil {
				t.Fatal(err)
			}
		}, want{false, 0, 1, 0, 0, 0}},
		{"skipped stale", 30 * time.Second, func(lc *ladderLaneCity, op opjournal.Op) {
			lc.closeOp(op.ID, opjournal.Outcome{Arm: opjournal.ArmSkipped, Reason: opjournal.ReasonStale})
		}, want{false, 0, 1, 0, 0, 0}},
		{"open before start_by", 30 * time.Second, func(*ladderLaneCity, opjournal.Op) {}, want{true, 1, 0, 0, 0, 0}},
		{"open past start_by", 3 * time.Minute, func(*ladderLaneCity, opjournal.Op) {}, want{false, 0, 1, 1, 0, 1}},
		{"in flight", 3 * time.Minute, func(lc *ladderLaneCity, op opjournal.Op) { lc.startOp(op.ID) }, want{true, 1, 0, 0, 0, 0}},
		// Past its bound it counts; the next due attempt, reserved the same
		// pass, waits on the op still in flight, so it is withdrawn.
		{"in flight past its bound", 5 * time.Minute, func(lc *ladderLaneCity, op opjournal.Op) { lc.startOp(op.ID) }, want{false, 1, 1, 0, 0, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lc := newLadderLaneCity(t, "message")
			id := lc.startedClaim()
			lc.at(0)
			lc.at(halfPast(1))
			op := lc.onlyOp()
			tc.do(lc, op)
			lc.at(halfPast(1) + tc.after)
			e := lc.entry(id)
			got := want{!e.Notice.IsZero(), e.Attempts, e.Paused, e.Refused, len(lc.events(events.WorkNudged)), len(lc.events(events.WorkNudgeFailed))}
			if got != tc.want {
				t.Fatalf("after the settling pass %+v, want %+v (entry %+v)", got, tc.want, e)
			}
			if n := lc.nudges(); n != 0 {
				t.Fatalf("typed %d nudges", n)
			}
		})
	}
}

func TestWorkLadderHoldsTheEntryAndTheSeatUntilItsNoticeSettles(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	started := lc.startedClaim()
	other := lc.claim("not started")
	lc.at(0)
	lc.at(halfPast(1))
	first := lc.onlyOp()
	lc.startOp(first.ID) // in flight: pending until it closes
	for d := halfPast(1) + 30*time.Second; d <= halfPast(4); d += 30 * time.Second {
		lc.at(d)
		if ops := lc.ladderOps(); len(ops) != 1 || ops[0].ID != first.ID {
			t.Fatalf("at %v open ladder notices %+v, want only the first", d, ops)
		}
	}
	var nudged, waiting string
	if first.Causation == started {
		nudged, waiting = started, other
	} else {
		nudged, waiting = other, started
	}
	if e := lc.entry(waiting); e.Attempts != 0 {
		t.Fatalf("the other entry %+v was nudged while a notice was pending", e)
	}
	lc.closeOp(first.ID, succeeded)
	lc.at(5 * time.Minute)
	if e := lc.entry(nudged); !e.Notice.IsZero() || e.Attempts != 1 {
		t.Fatalf("settled entry %+v", e)
	}
	op := lc.onlyOp()
	if op.Causation != waiting {
		t.Fatalf("next notice is about %s, want %s at its due time", op.Causation, waiting)
	}
}

// Over 8 hours of random outcomes and delays, the seat never has two open
// ladder notices, no settled mark's op is reserved again, and every mark is
// settled within its bound.
func TestWorkLadderKeepsOneLadderNoticePerSeat(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		t.Run(strconv.FormatUint(seed, 10), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 5))
			lc := newLadderLaneCity(t, "message")
			lc.startedClaim()
			lc.claim("not started")
			seen := map[string]bool{}
			settled := map[string]bool{}
			lc.run(8*time.Hour, 30*time.Second, func() {
				ops := lc.ladderOps()
				if len(ops) > 1 {
					t.Fatalf("at %v: %d open ladder notices", lc.now.Sub(lc.start), len(ops))
				}
				for _, e := range lc.cell().Entries {
					m := e.Notice
					if m.IsZero() {
						continue
					}
					if settled[m.Op] {
						t.Fatalf("op %s reserved again after it settled", m.Op)
					}
					seen[m.Op] = true
					if age := lc.now.Sub(m.At); age > ladderNoticeSettleBound+30*time.Second {
						t.Fatalf("mark %s unsettled for %v", m.Op, age)
					}
				}
				for id := range seen {
					if !markedWith(lc.cell(), id) {
						settled[id] = true
					}
				}
				for _, op := range ops {
					switch rng.IntN(6) {
					case 0:
						lc.closeOp(op.ID, succeeded)
					case 1:
						lc.closeOp(op.ID, failed(opjournal.ReasonExpiredBeforeStart))
					case 2:
						if op.Status == opjournal.StatusSubmitted {
							lc.startOp(op.ID)
						}
					case 3:
						if _, _, err := lc.st().Cancel(op.ID, "operator", "soak"); err != nil {
							t.Fatal(err)
						}
					}
				}
				if rng.IntN(40) == 0 { // the seat did some work
					lc.sp.SetActivity(ladderSeatName, lc.now)
				}
			})
			if len(seen) < 5 {
				t.Fatalf("%d notices in 8h: the soak tested little", len(seen))
			}
		})
	}
}

// markedWith reports whether a mark in r names op.
func markedWith(r workladder.Record, op string) bool {
	for _, e := range r.Entries {
		if e.Notice.Op == op {
			return true
		}
	}
	return false
}

// L1 on the lane: every notice delivered right after its proposal keeps the
// wedge replay's times, recycle and escalation, and types nothing.
func TestWorkLadderReplaysTheWedgeOnTheLane(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.startedClaim()
	var proposed []time.Duration
	lc.run(8*time.Hour, 30*time.Second, func() {
		for _, op := range lc.ladderOps() {
			proposed = append(proposed, op.CreatedAt.Sub(lc.start))
			lc.closeOp(op.ID, succeeded)
		}
		if len(lc.posted) > 0 && lc.row().Metadata["generation"] == "7" {
			lc.setRow(map[string]string{"generation": "8", "instance_token": "tok-8"})
		}
	})
	want := []time.Duration{
		halfPast(1), halfPast(4), halfPast(10), halfPast(22), halfPast(46), halfPast(94),
		halfPast(156), halfPast(159), halfPast(165), halfPast(177), halfPast(201), halfPast(249),
		halfPast(309), halfPast(369), halfPast(429),
	}
	if fmt.Sprint(proposed) != fmt.Sprint(want) {
		t.Fatalf("notices at\n %v\nwant\n %v", proposed, want)
	}
	if n := lc.nudges(); n != 0 {
		t.Fatalf("typed %d nudges", n)
	}
	if len(lc.posted) != 1 || lc.posted[0].Generation != "7" {
		t.Fatalf("recycles %+v, want one at generation 7", lc.posted)
	}
	if got := escalations(lc.ladderCity); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertStartedRecycled, workladder.AlertStartedUnanswered}) {
		t.Fatalf("escalations %v", got)
	}
	if e := lc.entry(id); e.Rung != workladder.RungEscalated {
		t.Fatalf("entry %+v, want escalated", e)
	}
}

// A seat whose every notice expires undelivered is reminded every PausedCap,
// alerted once, and its unstarted claim is never released on reminders it
// never saw.
func TestWorkLadderBacksOffASeatThatCannotTakeItsNotices(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.claim("never started")
	var proposed []time.Duration
	lc.run(8*time.Hour, 30*time.Second, func() {
		for _, op := range lc.ladderOps() {
			proposed = append(proposed, op.CreatedAt.Sub(lc.start))
			lc.closeOp(op.ID, failed(opjournal.ReasonExpiredBeforeStart))
		}
	})
	if len(proposed) < 8*60/11 {
		t.Fatalf("%d notices in 8h, want one about every 10 minutes", len(proposed))
	}
	for i := 1; i < len(proposed); i++ {
		if gap := proposed[i] - proposed[i-1]; gap < workladder.DefaultPausedCap {
			t.Fatalf("notices %v and %v %v apart, want >= PausedCap", proposed[i-1], proposed[i], gap)
		}
	}
	if got := escalations(lc.ladderCity); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertNudgeRefused}) {
		t.Fatalf("escalations %v, want one nudge_refused", got)
	}
	if b, _ := lc.work.Get(id); b.Status != "in_progress" || len(lc.events(events.WorkReleased)) != 0 {
		t.Fatalf("claim %q released on reminders the seat never saw", b.Status)
	}
}

// ladderGateMeta are the row writes that hold the ladder's gate.
func ladderGateMeta() map[string]func(now time.Time) map[string]string {
	return map[string]func(now time.Time) map[string]string{
		"stop requested": func(now time.Time) map[string]string {
			return map[string]string{session.DrainIntentReasonKey: drainOrphaned, session.DrainIntentAtKey: now.Format(time.RFC3339), session.DrainIntentIncarnationKey: "7"}
		},
		"content held": func(now time.Time) map[string]string {
			return map[string]string{session.ContentHoldKey: session.ContentHold{By: "operator", At: now}.String()}
		},
		"kill pending": func(now time.Time) map[string]string {
			return map[string]string{"state": "asleep", "state_reason": session.KillPendingReason, "sleep_reason": "killed", "slept_at": now.Format(time.RFC3339)}
		},
		"heartbeat": func(now time.Time) map[string]string {
			return map[string]string{"held_until": now.Add(6 * time.Hour).Format(time.RFC3339)}
		},
		"agent wait":      func(time.Time) map[string]string { return map[string]string{"wait_hold": "true"} },
		"restart pending": func(time.Time) map[string]string { return map[string]string{"restart_requested": "true"} },
		"operator hold":   func(time.Time) map[string]string { return map[string]string{"sleep_intent": "user-hold"} },
		"idle stop pending": func(time.Time) map[string]string {
			return map[string]string{"sleep_intent": "idle-stop-pending"}
		},
	}
}

// A seat whose idle stop is pending is held: the lane would hold and expire
// its notices (the content gate's stop_pending), so the ladder proposes
// none, types none, and reads no refusal off the seat however long it stays.
func TestWorkLadderProposesNoNoticeToAnIdleStopPendingSeat(t *testing.T) {
	for name, claim := range map[string]func(lc *ladderLaneCity) string{
		"started":   func(lc *ladderLaneCity) string { return lc.startedClaim() },
		"unstarted": func(lc *ladderLaneCity) string { return lc.claim("unstarted") },
	} {
		t.Run(name, func(t *testing.T) {
			lc := newLadderLaneCity(t, "message")
			id := claim(lc)
			lc.setRow(map[string]string{"sleep_intent": "idle-stop-pending"})
			lc.run(2*time.Hour, 30*time.Second, func() {
				if ops := lc.ladderOps(); len(ops) != 0 {
					t.Fatalf("open ladder notices %+v to an idle-stop-pending seat", ops)
				}
			})
			if n := lc.nudges(); n != 0 {
				t.Fatalf("typed %d nudges into an idle-stop-pending seat", n)
			}
			if got := escalations(lc.ladderCity); len(got) != 0 {
				t.Fatalf("escalations %v, want none: a held seat refuses nothing", got)
			}
			if b, _ := lc.work.Get(id); b.Status != "in_progress" || len(lc.events(events.WorkReleased)) != 0 {
				t.Fatalf("claim %q released under a held gate", b.Status)
			}
			for _, e := range lc.cell().Entries {
				if e.Attempts != 0 || e.Refused != 0 || !e.Notice.IsZero() {
					t.Fatalf("entry %+v, want nothing reserved under a held gate", e)
				}
			}
		})
	}
}

func TestWorkLadderWithdrawsItsNoticeWhenItsGateHolds(t *testing.T) {
	for name, meta := range ladderGateMeta() {
		t.Run(name, func(t *testing.T) {
			lc := newLadderLaneCity(t, "message")
			id := lc.startedClaim()
			lc.at(0)
			lc.at(halfPast(1))
			op := lc.onlyOp()
			lc.setRow(meta(lc.now))
			lc.at(2 * time.Minute)
			got := lc.op(op.ID)
			if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != ladderNoticePrincipal {
				t.Fatalf("op %s %+v cancel %+v, want canceled by the ladder", got.Status, got.Outcome, got.Cancel)
			}
			if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 0 {
				t.Fatalf("entry %+v, want withdrawn: re-counted paused, no refusal", e)
			}
			if n := len(lc.events(events.WorkNudgeFailed)) + len(lc.events(events.WorkNudged)); n != 0 {
				t.Fatalf("%d nudge events for a withdrawn notice", n)
			}
		})
	}
	t.Run("never proposed", func(t *testing.T) {
		lc := newLadderLaneCity(t, "message")
		id := lc.startedClaim()
		lc.at(0)
		e := lc.entry(id)
		e.Attempts, e.Last = 1, lc.now
		e.Notice = workladder.NoticeMark{Op: "gc-op-neverproposed", At: lc.now}
		lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{e}}), "wait_hold": "true"})
		lc.at(time.Minute)
		if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 0 {
			t.Fatalf("entry %+v, want withdrawn while the gate holds, not refused", e)
		}
		if n := len(lc.events(events.WorkNudgeFailed)); n != 0 {
			t.Fatalf("work.nudge_failed = %d for a withdrawn notice", n)
		}
	})
	t.Run("in flight", func(t *testing.T) {
		lc := newLadderLaneCity(t, "message")
		id := lc.startedClaim()
		lc.at(0)
		lc.at(halfPast(1))
		op := lc.onlyOp()
		lc.startOp(op.ID)
		lc.setRow(map[string]string{"held_until": lc.now.Add(time.Hour).Format(time.RFC3339)})
		lc.at(2 * time.Minute)
		if got := lc.op(op.ID); got.Status != opjournal.StatusExecuting || got.Cancel.By != ladderNoticePrincipal {
			t.Fatalf("op %s cancel %+v, want the cancel requested", got.Status, got.Cancel)
		}
		if e := lc.entry(id); e.Notice.Op != op.ID {
			t.Fatalf("entry %+v, want the mark pending", e)
		}
		lc.closeOp(op.ID, opjournal.Outcome{Arm: opjournal.ArmCanceled})
		lc.at(2*time.Minute + 30*time.Second)
		if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 {
			t.Fatalf("entry %+v, want withdrawn on the lane's close", e)
		}
	})
}

func TestWorkLadderWithdrawsTheNoticeOfWorkThatEnded(t *testing.T) {
	for name, end := range map[string]func(lc *ladderLaneCity, id string){
		"claim closed": func(lc *ladderLaneCity, id string) {
			closed := "closed"
			if err := lc.work.Update(id, beads.UpdateOpts{Status: &closed}); err != nil {
				t.Fatal(err)
			}
		},
		"agent started it": func(lc *ladderLaneCity, id string) {
			key := lc.entry(id).Key
			lc.setRow(map[string]string{session.WorkStartedKey: workladder.EncodeStarted(workladder.Started{V: 1, Claims: []workladder.StartedClaim{{Key: key, Gen: 7, At: lc.now, Via: workladder.ViaClaim}}})})
		},
		"new generation": func(lc *ladderLaneCity, _ string) {
			lc.setRow(map[string]string{"generation": "8", "instance_token": "tok-8"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			lc := newLadderLaneCity(t, "message")
			id := lc.claim("unstarted")
			lc.at(0)
			lc.at(halfPast(1))
			op := lc.onlyOp()
			end(lc, id)
			lc.at(2 * time.Minute)
			if got := lc.op(op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled {
				t.Fatalf("op %s %+v, want withdrawn", got.Status, got.Outcome)
			}
			for _, e := range lc.cell().Entries {
				if !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 0 {
					t.Fatalf("entry %+v: the new episode's counters were touched", e)
				}
			}
		})
	}
}

// A commit that landed a mark whose op was never proposed (a crash between
// the two) is settled refused on the next pass, and the next attempt gets a
// key of its own.
func TestWorkLadderNoticeLostToACrashIsRefused(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	op := lc.onlyOp()
	if _, _, err := lc.st().Cancel(op.ID, "test", "the crash never proposed it"); err != nil {
		t.Fatal(err)
	}
	cell := lc.cell()
	lost := op.ID + "-lost"
	cell.Entries[0].Notice.Op = lost
	lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(cell)})
	lc.at(2 * time.Minute)
	if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 1 {
		t.Fatalf("entry %+v, want refused", e)
	}
	fails := lc.events(events.WorkNudgeFailed)
	if len(fails) != 1 || !strings.Contains(fails[0].Message, "notice not proposed") {
		t.Fatalf("work.nudge_failed = %+v", fails)
	}
	if !strings.Contains(string(fails[0].Payload), `"attempt":1`) {
		t.Fatalf("work.nudge_failed payload %s, want the reserved attempt 1, as a direct refusal reports", fails[0].Payload)
	}
	lc.run(15*time.Minute, 30*time.Second, nil)
	next := lc.onlyOp()
	if next.ID == op.ID || next.ClientKey == op.ClientKey {
		t.Fatalf("next attempt reused %s", op.ClientKey)
	}
	if e := lc.entry(id); next.ClientKey != wantLadderKey("tok-7", e, 2) {
		t.Fatalf("next key %q, want ordinal 2", next.ClientKey)
	}
}

// A notice lost to a crash and settled on the pass that first sees the
// seat's next generation is reported at the generation it was reserved in,
// not the one its fold lands at: its op, never proposed, names none.
func TestWorkLadderLostNoticeReportsItsReservedGeneration(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	op := lc.onlyOp()
	if _, _, err := lc.st().Cancel(op.ID, "test", "the crash never proposed it"); err != nil {
		t.Fatal(err)
	}
	cell := lc.cell()
	cell.Entries[0].Notice.Op = op.ID + "-lost"
	lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(cell), "generation": "8", "instance_token": "tok-8"})
	lc.at(2 * time.Minute)
	fails := lc.events(events.WorkNudgeFailed)
	if len(fails) != 1 {
		t.Fatalf("work.nudge_failed = %+v, want the lost notice's", fails)
	}
	var pl events.WorkLadderPayload
	if err := json.Unmarshal(fails[0].Payload, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Generation != 7 || pl.Attempt != 1 {
		t.Fatalf("work.nudge_failed payload %+v, want attempt 1 at generation 7", pl)
	}
}

func TestWorkLadderNoticeProposalFailures(t *testing.T) {
	t.Run("the journal cannot be listed", func(t *testing.T) {
		var fs *failingOpListStore
		lc := newLadderLaneCityOver(t, func(s laneTestStore) beads.Store {
			fs = &failingOpListStore{laneTestStore: s}
			return fs
		}, "message")
		id := lc.startedClaim()
		lc.at(0)
		fs.fail.Store(true)
		lc.at(halfPast(1))
		fs.fail.Store(false)
		if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 1 {
			t.Fatalf("entry %+v, want refused at once", e)
		}
		if n := len(lc.events(events.WorkNudgeFailed)); n != 1 {
			t.Fatalf("work.nudge_failed = %d", n)
		}
	})
	t.Run("a long outage keeps one failure tally per seat", func(t *testing.T) {
		var fs *failingOpListStore
		lc := newLadderLaneCityOver(t, func(s laneTestStore) beads.Store {
			fs = &failingOpListStore{laneTestStore: s}
			return fs
		}, "message")
		lc.startedClaim()
		fs.fail.Store(true)
		lc.run(2*time.Hour, 30*time.Second, nil)
		if n := len(lc.events(events.WorkNudgeFailed)); n < 3 {
			t.Fatalf("work.nudge_failed = %d, want a failure per attempt", n)
		}
		lc.host.mu.Lock()
		tallies := len(lc.host.noticeFailures)
		lc.host.mu.Unlock()
		if tallies != 1 {
			t.Fatalf("%d failure tallies after an outage, want one for the seat", tallies)
		}
	})
	t.Run("the configured nudge is too large", func(t *testing.T) {
		lc := newLadderLaneCity(t, "message")
		lc.env.Cfg.Agents[0].Nudge = strings.Repeat("x", opjournal.MaxInlinePayload+1)
		id := lc.claim("unstarted")
		lc.at(0)
		lc.at(halfPast(1))
		if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 1 || e.Refused != 0 {
			t.Fatalf("entry %+v, want counted as failed", e)
		}
		if n := len(lc.events(events.WorkNudgeFailed)); n != 1 {
			t.Fatalf("work.nudge_failed = %d", n)
		}
	})
}

func TestWorkLadderUnsettledNoticeCountsPastItsBound(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	op := lc.onlyOp()
	lc.startOp(op.ID)
	lc.at(halfPast(1) + ladderNoticeSettleBound - time.Second)
	if e := lc.entry(id); e.Notice.Op != op.ID {
		t.Fatalf("entry %+v, want pending inside its bound", e)
	}
	lc.at(halfPast(1) + ladderNoticeSettleBound)
	fails := lc.events(events.WorkNudgeFailed)
	if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts < 1 || len(fails) != 1 || !strings.Contains(fails[0].Message, "notice unsettled") {
		t.Fatalf("entry %+v, failures %+v; want counted failed past the bound", e, fails)
	}
}

// ladderRuntimeReadFailing fails reads of one id.
type ladderRuntimeReadFailing struct {
	laneTestStore
	id string
}

func (s ladderRuntimeReadFailing) Get(id string) (beads.Bead, error) {
	if id == s.id {
		return beads.Bead{}, errors.New("read unavailable")
	}
	return s.laneTestStore.Get(id)
}

// newUnreadableRouteCity is a ladderLaneCity whose seat runs in protocol mode
// and whose runtime row cannot be read: its notice route is unreadable.
func newUnreadableRouteCity(t *testing.T) *ladderLaneCity {
	t.Helper()
	var fs *ladderRuntimeReadFailing
	lc := newLadderLaneCityOver(t, func(s laneTestStore) beads.Store {
		fs = &ladderRuntimeReadFailing{laneTestStore: s}
		return fs
	}, "message")
	rid, err := opjournal.RuntimeRowID("gc", lc.seat)
	if err != nil {
		t.Fatal(err)
	}
	fs.id = rid
	lc.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	return lc
}

// A route that cannot be read refuses the seat's due nudge, as a seat that
// cannot take one does: nothing is typed or proposed, the attempt is
// re-counted as paused, and the refusal is reported.
func TestWorkLadderUnreadableRouteRefusesTheNudge(t *testing.T) {
	lc := newUnreadableRouteCity(t)
	id := lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	lc.at(2 * time.Minute)
	if n, ops := lc.nudges(), lc.ladderOps(); n != 0 || len(ops) != 0 {
		t.Fatalf("%d nudges, ops %+v: nudged blind", n, ops)
	}
	if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 1 {
		t.Fatalf("entry %+v, want the attempt refused: re-counted paused, unmarked", e)
	}
	fails := lc.events(events.WorkNudgeFailed)
	if len(fails) != 1 || !strings.Contains(fails[0].Message, "notice route unreadable") {
		t.Fatalf("work.nudge_failed = %+v, want one naming the route", fails)
	}
}

// setAtThreshold writes the seat's cell with bead's entry changed by set and
// last nudged two hours ago, with no activity on the seat since.
func (lc *ladderLaneCity) setAtThreshold(bead string, set func(e *workladder.Entry)) {
	lc.t.Helper()
	e := lc.entry(bead)
	e.Last = lc.now.Add(-2 * time.Hour)
	set(&e)
	lc.sp.SetActivity(ladderSeatName, lc.now.Add(-3*time.Hour)) // no progress since: no reset
	lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{e}})})
}

// A route that cannot be read refuses only the nudge: the recycle, the
// release and the alerts decided with it still run.
func TestWorkLadderUnreadableRouteStillRecyclesReleasesAndAlerts(t *testing.T) {
	for name, tc := range map[string]struct {
		seed func(lc *ladderLaneCity) string
		// refused: the nudge decided with the action was refused (not
		// released with its claim).
		refused bool
		check   func(t *testing.T, lc *ladderLaneCity, id string)
	}{
		"a trigger at its threshold is recycled": {
			seed: func(lc *ladderLaneCity) string {
				b, err := lc.work.Create(beads.Bead{Title: "trigger", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
				if err != nil {
					lc.t.Fatal(err)
				}
				lc.trigger = b.ID
				lc.setRow(map[string]string{beadmeta.TriggerBeadIDMetadataKey: b.ID})
				lc.at(0)
				lc.setAtThreshold(b.ID, func(e *workladder.Entry) { e.Attempts = workladder.DefaultTriggerThreshold })
				return b.ID
			},
			refused: true,
			check: func(t *testing.T, lc *ladderLaneCity, _ string) {
				if len(lc.posted) != 1 || len(lc.events(events.WorkRecycleRequested)) != 1 {
					t.Fatalf("recycles posted %+v, want the trigger's", lc.posted)
				}
			},
		},
		"an unfinished release is retried, with its alert": {
			seed: func(lc *ladderLaneCity) string {
				id := lc.claim("never started")
				lc.at(0)
				lc.setAtThreshold(id, func(e *workladder.Entry) { e.Attempts, e.Unreleased = workladder.DefaultClaimedThreshold, true })
				return id
			},
			check: func(t *testing.T, lc *ladderLaneCity, id string) {
				if b, _ := lc.work.Get(id); b.Status != "open" {
					t.Fatalf("claim %q, want released", b.Status)
				}
				if got := escalations(lc.ladderCity); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertReleaseUnfinished}) {
					t.Fatalf("escalations %v, want release_unfinished", got)
				}
			},
		},
		"a started claim past its recycles escalates": {
			seed: func(lc *ladderLaneCity) string {
				id := lc.startedClaim()
				lc.at(0)
				lc.setAtThreshold(id, func(e *workladder.Entry) {
					e.Attempts, e.Recycles = workladder.DefaultStartedThreshold, workladder.DefaultMaxRecycles
				})
				return id
			},
			refused: true,
			check: func(t *testing.T, lc *ladderLaneCity, id string) {
				if e := lc.entry(id); e.Rung != workladder.RungEscalated {
					t.Fatalf("entry %+v, want escalated", e)
				}
				if got := escalations(lc.ladderCity); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertStartedUnanswered}) {
					t.Fatalf("escalations %v, want started_unanswered", got)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			lc := newUnreadableRouteCity(t)
			id := tc.seed(lc)
			lc.at(time.Minute)
			tc.check(t, lc, id)
			if n, ops := lc.nudges(), lc.ladderOps(); n != 0 || len(ops) != 0 {
				t.Fatalf("%d nudges, ops %+v: nudged blind", n, ops)
			}
			want := 0
			if tc.refused {
				want = 1
				if e := lc.entry(id); e.Paused != 1 || e.Refused != 1 {
					t.Fatalf("entry %+v, want its nudge refused", e)
				}
			}
			if n := len(lc.events(events.WorkNudgeFailed)); n != want {
				t.Fatalf("work.nudge_failed = %d, want %d", n, want)
			}
		})
	}
}

// Over 8 hours on a route that cannot be read, a started claim is still
// paced: every due nudge is refused, at most every PausedCap, and reported;
// the seat is alerted once, and it is not recycled on reminders it never got.
func TestWorkLadderUnreadableRouteKeepsPacingTheSeat(t *testing.T) {
	lc := newUnreadableRouteCity(t)
	id := lc.startedClaim()
	var refused []time.Duration
	lc.run(8*time.Hour, 30*time.Second, func() {
		if n := len(lc.events(events.WorkNudgeFailed)); n > len(refused) {
			refused = append(refused, lc.now.Sub(lc.start))
		}
	})
	if n, ops := lc.nudges(), lc.ladderOps(); n != 0 || len(ops) != 0 {
		t.Fatalf("%d nudges, ops %+v: nudged blind", n, ops)
	}
	if len(refused) < 8*60/11 {
		t.Fatalf("%d refused nudges in 8h, want one about every 10 minutes", len(refused))
	}
	for i := 1; i < len(refused); i++ {
		if gap := refused[i] - refused[i-1]; gap < workladder.DefaultPausedCap {
			t.Fatalf("refusals at %v and %v, %v apart, want >= PausedCap", refused[i-1], refused[i], gap)
		}
	}
	if got := escalations(lc.ladderCity); fmt.Sprint(got) != fmt.Sprint([]string{workladder.AlertNudgeRefused}) {
		t.Fatalf("escalations %v, want one nudge_refused", got)
	}
	if e := lc.entry(id); len(lc.posted) != 0 || e.Attempts != 0 {
		t.Fatalf("entry %+v, recycles %+v: counted reminders the seat never got", e, lc.posted)
	}
}

// ladderStampHidingStore serves the seat's runtime row (its host stamp) to
// the first shown reads and hides it from every later one: the route the
// pass resolves sees a host serving the seat, the proposal after the commit
// does not.
type ladderStampHidingStore struct {
	laneTestStore
	id           string
	shown, reads int
}

func (s *ladderStampHidingStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		if s.reads++; s.reads > s.shown {
			return beads.Bead{}, beads.ErrNotFound
		}
	}
	return s.laneTestStore.Get(id)
}

// A route that changes between the commit and the proposal (here the host
// stamp is gone when the proposal reads it) withdraws the attempt: it is
// re-counted as paused, with no refusal and no event, and nothing is typed.
func TestWorkLadderNoticeRouteChangedAfterTheCommitIsWithdrawn(t *testing.T) {
	var fs *ladderStampHidingStore
	lc := newLadderLaneCityOver(t, func(s laneTestStore) beads.Store {
		fs = &ladderStampHidingStore{laneTestStore: s, shown: 1}
		return fs
	}, "message")
	rid, err := opjournal.RuntimeRowID("gc", lc.seat)
	if err != nil {
		t.Fatal(err)
	}
	fs.id = rid
	lc.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	stampHost(t, lc.f, lc.st(), 7, opjournal.KindMessage)
	id := lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	if fs.reads < 2 {
		t.Fatalf("the runtime row was read %d times, want the route's read and the proposal's", fs.reads)
	}
	if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 0 {
		t.Fatalf("entry %+v, want the attempt withdrawn: re-counted paused, no refusal", e)
	}
	if n := len(lc.events(events.WorkNudgeFailed)) + len(lc.events(events.WorkNudged)); n != 0 {
		t.Fatalf("%d nudge events for a withdrawn attempt", n)
	}
	if n, ops := lc.nudges(), lc.ladderOps(); n != 0 || len(ops) != 0 {
		t.Fatalf("%d nudges, ops %+v, want neither", n, ops)
	}
}

// A proposal whose op is not the attempt's mark (an invariant breach: the
// commit and the proposal derive the id from the same row and entry) cancels
// that op and withdraws the mark, so no notice the cell does not track
// reaches the seat.
func TestWorkLadderNoticeNotItsMarkIsWithdrawn(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.startedClaim()
	lc.at(0)
	e := lc.entry(id)
	e.Attempts, e.Last = 1, lc.now
	e.Notice = workladder.NoticeMark{Op: "gc-op-notitsmark", At: lc.now}
	lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{e}, LastNudge: lc.now})})
	p := &ladderPass{l: lc.lane, env: lc.env, cfg: lc.env.Cfg, now: lc.now, front: sessionFrontDoor(lc.env.CityStore)}
	row := lc.row()
	route := p.route(row)
	if !route.viaLane() {
		t.Fatalf("route %+v, want the lane", route)
	}
	p.proposeNotice(row, workladder.Nudge{Key: e.Key, Kind: e.Kind, Rung: e.Rung, Attempt: 1}, e, 7, e.Notice, route)
	proposed, err := ladderNoticeOpID(lc.st(), row, e.Key, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := lc.op(proposed); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != ladderNoticePrincipal {
		t.Fatalf("op %s %+v cancel %+v, want canceled by the ladder", got.Status, got.Outcome, got.Cancel)
	}
	if got := lc.entry(id); !got.Notice.IsZero() || got.Attempts != 0 || got.Paused != 1 || got.Refused != 0 {
		t.Fatalf("entry %+v, want the mark withdrawn", got)
	}
}

func TestWorkLadderNoticeDeliveredByTheLaneHost(t *testing.T) {
	for name, setup := range map[string]func(lc *ladderLaneCity) (string, string){
		"trigger": func(lc *ladderLaneCity) (string, string) {
			b, err := lc.work.Create(beads.Bead{Title: "trigger", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
			if err != nil {
				t.Fatal(err)
			}
			lc.trigger = b.ID
			lc.setRow(map[string]string{beadmeta.TriggerBeadIDMetadataKey: b.ID})
			return b.ID, defaultPoolClaimNudge
		},
		"claimed": func(lc *ladderLaneCity) (string, string) { return lc.claim("unstarted"), ladderClaimedNudge },
		"started": func(lc *ladderLaneCity) (string, string) {
			id := lc.startedClaim()
			return id, fmt.Sprintf(ladderStartedNudge, id)
		},
	} {
		t.Run(name, func(t *testing.T) {
			lc := newLadderLaneCity(t, "message")
			_, text := setup(lc)
			lc.at(0)
			lc.at(halfPast(1))
			op := lc.onlyOp()
			delivered, err := opjournal.RenderDelivery([]opjournal.Op{op}, opjournal.TextReminder, false)
			if err != nil {
				t.Fatal(err)
			}
			if want := reminderGolden("planner", text); delivered != want {
				t.Fatalf("delivered %q, want %q", delivered, want)
			}
			leaks := []string{op.ID, op.ClientKey, ladderNoticePrincipal}
			if !strings.Contains(text, lc.seat) { // the test's bead and seat ids may coincide
				leaks = append(leaks, lc.seat)
			}
			for _, leak := range leaks {
				if strings.Contains(delivered, leak) {
					t.Fatalf("delivered text names %q", leak)
				}
			}
			lc.closeOp(op.ID, succeeded)
			lc.at(2 * time.Minute)
			if n := len(lc.events(events.WorkNudged)); n != 1 {
				t.Fatalf("work.nudged = %d after delivery, want 1", n)
			}
		})
	}
}

// The unfinished-release branch decides a release and a nudge for one key:
// the release lands, so the nudge's notice is never proposed and its mark is
// cleared.
func TestWorkLadderReleasedClaimProposesNoNotice(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.claim("never started")
	lc.at(0)
	e := lc.entry(id)
	e.Attempts, e.Unreleased, e.Last = 3, true, lc.now.Add(-2*time.Hour)
	lc.sp.SetActivity(ladderSeatName, lc.now.Add(-3*time.Hour)) // no progress since: no reset
	lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{e}})})
	var log strings.Builder
	lc.lane.stderr = &log
	lc.at(time.Minute)
	if b, _ := lc.work.Get(id); b.Status != "open" {
		t.Fatalf("claim %q, want released; log:\n%s\ncell %s", b.Status, log.String(), lc.row().Metadata[session.WorkLadderKey])
	}
	if ops := lc.ladderOps(); len(ops) != 0 || lc.nudges() != 0 {
		t.Fatalf("ops %+v, nudges %d: a released claim was reminded", ops, lc.nudges())
	}
	for _, e := range lc.cell().Entries {
		if !e.Notice.IsZero() {
			t.Fatalf("entry %+v keeps a dangling mark", e)
		}
	}
}

func TestWorkLadderPausedNoticeRefusalRecordsNothing(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.startedClaim()
	writeHealth := func() {
		dir := filepath.Join(lc.env.CityPath, ".gc", "cache")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(providerHealthFileFormat{Providers: []providerHealthRecord{{Provider: "claude", Status: "unhealthy", ProbedAt: float64(time.Now().Unix())}}})
		if err := os.WriteFile(filepath.Join(dir, "provider-health.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeHealth()
	proposed := 0
	lc.run(time.Hour, 30*time.Second, func() {
		writeHealth()
		for _, op := range lc.ladderOps() {
			proposed++
			lc.closeOp(op.ID, failed(opjournal.ReasonExpiredBeforeStart))
		}
	})
	if proposed == 0 {
		t.Fatal("no paused notice was proposed")
	}
	if e := lc.entry(id); e.Refused != 0 || e.Attempts != 0 || len(escalations(lc.ladderCity)) != 0 {
		t.Fatalf("entry %+v, escalations %v: a paused notice's refusal was recorded", e, escalations(lc.ladderCity))
	}
}

// Unlisting message while a notice is pending: the ladder reads the mark
// through the live op store with no host, holds the entry until it settles,
// and only then nudges directly.
func TestWorkLadderSettlesItsNoticeAfterMessageIsUnlisted(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	op := lc.onlyOp()
	lc.startOp(op.ID)
	lc.stopHost()
	lc.at(halfPast(4))
	if n := lc.nudges(); n != 0 {
		t.Fatalf("typed %d nudges while the notice may still land", n)
	}
	lc.closeOp(op.ID, succeeded)
	lc.at(5 * time.Minute)
	if n := len(lc.events(events.WorkNudged)); n != 2 {
		t.Fatalf("work.nudged = %d, want the notice's and the direct nudge's", n)
	}
	if n := lc.nudges(); n != 1 {
		t.Fatalf("typed %d nudges, want the direct one once settled", n)
	}
	if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 2 {
		t.Fatalf("entry %+v", e)
	}
}

// [daemon] work_ladder = "off" withdraws the ladder's open notices on the
// pass that hands the seats back to the backstops, in the CAS that drops a
// pending release and on a cell that holds none, so the lane types no ladder
// reminder beside the backstops' nudges. A notice in flight keeps the cancel
// as a request, and its mark until the lane decides it.
func TestWorkLadderOffWithdrawsItsNotices(t *testing.T) {
	marked := func(t *testing.T) (*ladderLaneCity, string, opjournal.Op) {
		t.Helper()
		lc := newLadderLaneCity(t, "message")
		id := lc.startedClaim()
		lc.at(0)
		lc.at(halfPast(1))
		return lc, id, lc.onlyOp()
	}
	off := func(lc *ladderLaneCity, d time.Duration) {
		lc.env.Cfg.Daemon.WorkLadder = "off"
		lc.at(d)
	}
	withdrawn := func(t *testing.T, lc *ladderLaneCity, id, op string) {
		t.Helper()
		if got := lc.op(op); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != ladderNoticePrincipal {
			t.Fatalf("op %s %+v cancel %+v, want canceled by the ladder", got.Status, got.Outcome, got.Cancel)
		}
		if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 0 {
			t.Fatalf("entry %+v, want the mark withdrawn: re-counted paused, no refusal", e)
		}
	}
	t.Run("pending", func(t *testing.T) {
		lc, id, op := marked(t)
		off(lc, 2*time.Minute)
		withdrawn(t, lc, id, op.ID)
		if ops := lc.ladderOps(); len(ops) != 0 {
			t.Fatalf("open ladder notices %+v with the ladder off", ops)
		}
	})
	t.Run("with a pending release", func(t *testing.T) {
		lc, id, op := marked(t)
		key := workladder.ClaimKey{Store: lc.entry(id).Key.Store, Bead: lc.claim("being released")}
		cell := lc.cell()
		cell.Release = &workladder.ReleaseMark{Key: key, Gen: 7, At: lc.now}
		lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(cell)})
		off(lc, 2*time.Minute)
		withdrawn(t, lc, id, op.ID)
		if session.WorkLadderReleases(lc.row().Metadata[session.WorkLadderKey], key.Store, key.Bead) {
			t.Fatal("the pending release outlived the ladder")
		}
	})
	t.Run("never proposed", func(t *testing.T) {
		lc := newLadderLaneCity(t, "message")
		id := lc.startedClaim()
		lc.at(0)
		e := lc.entry(id)
		e.Attempts, e.Last = 1, lc.now
		e.Notice = workladder.NoticeMark{Op: "gc-op-neverproposed", At: lc.now}
		lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{e}})})
		off(lc, time.Minute)
		if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 0 {
			t.Fatalf("entry %+v, want the lost notice withdrawn, not refused", e)
		}
	})
	t.Run("in flight", func(t *testing.T) {
		lc, id, op := marked(t)
		lc.startOp(op.ID)
		off(lc, 2*time.Minute)
		if got := lc.op(op.ID); got.Status != opjournal.StatusExecuting || got.Cancel.By != ladderNoticePrincipal {
			t.Fatalf("op %s cancel %+v, want the cancel requested", got.Status, got.Cancel)
		}
		if e := lc.entry(id); e.Notice.Op != op.ID {
			t.Fatalf("entry %+v, want the mark kept while the lane decides", e)
		}
		lc.closeOp(op.ID, opjournal.Outcome{Arm: opjournal.ArmCanceled})
		lc.at(2*time.Minute + 30*time.Second)
		if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 {
			t.Fatalf("entry %+v, want withdrawn on the lane's close", e)
		}
	})
}

// snapshotWaitStore makes the step's snapshot (List) read the seat's row as
// holding an agent wait while the commit's fresh read (Get) does not: the
// seat's snapshot decision nudges nothing, so no route is resolved, and the
// commit's fresh decision nudges.
type snapshotWaitStore struct {
	laneTestStore
	on bool
}

func (s *snapshotWaitStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	bs, err := s.laneTestStore.List(q)
	if err != nil || !s.on {
		return bs, err
	}
	for i := range bs {
		if strings.TrimSpace(bs[i].Metadata["session_name"]) == ladderSeatName {
			m := maps.Clone(bs[i].Metadata)
			m["wait_hold"] = "true"
			bs[i].Metadata = m
		}
	}
	return bs, nil
}

// The commit holds a nudge whose route this pass never resolved and decides
// again; a release first decided there is re-proved against the seeds on
// the fresh row like any other: a claim the hook's current-claim stamp names
// started is not released, though holding the nudge freed the release slot.
func TestWorkLadderCommitReprovesAReleaseDecidedAfterANudgeHold(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("snapshot_wait=%v", race), func(t *testing.T) {
			var fs *snapshotWaitStore
			lc := newLadderLaneCityOver(t, func(s laneTestStore) beads.Store {
				fs = &snapshotWaitStore{laneTestStore: s}
				return fs
			}, "message")
			unfinished := lc.claim("release unfinished")
			stamped := lc.claim("started per the hook's stamp")
			lc.at(0)
			cell := lc.cell()
			for i := range cell.Entries {
				e := &cell.Entries[i]
				switch e.Key.Bead {
				case unfinished:
					e.Attempts, e.Unreleased, e.Last = 3, true, lc.now.Add(-3*time.Hour)
				case stamped:
					e.Attempts, e.Last = 3, lc.now.Add(-2*time.Hour)
				}
			}
			lc.sp.SetActivity(ladderSeatName, lc.now.Add(-4*time.Hour))
			lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(cell), beadmeta.CurrentClaimBeadIDMetadataKey: stamped})
			lc.claim("new work") // a new entry: the snapshot's decision changes the cell, so it commits
			fs.on = race
			var log strings.Builder
			lc.lane.stderr = &log
			lc.at(time.Minute)
			if b, _ := lc.work.Get(stamped); b.Status != "in_progress" {
				t.Fatalf("claim %s %q: released though current_claim_bead_id names it started; log:\n%s", stamped, b.Status, log.String())
			}
		})
	}
}

// seatReadFailingStore is a snapshotWaitStore whose Get of the seat's row
// fails while fail is set: a live read of the row that does not answer.
type seatReadFailingStore struct {
	*snapshotWaitStore
	seat string
	fail bool
}

func (s *seatReadFailingStore) Get(id string) (beads.Bead, error) {
	if s.fail && id == s.seat {
		return beads.Bead{}, errors.New("reading the row failed")
	}
	return s.snapshotWaitStore.Get(id)
}

// The held-gate withdraw cannot be undone, so the gate the pass's snapshot
// holds is proved again on a live read of the row first: a hold the row no
// longer carries (here an agent's wait that ended since the snapshot)
// withdraws nothing and leaves the notice to the lane, and a read that does
// not answer leaves the seat for the next pass.
func TestWorkLadderReprovesItsGateBeforeWithdrawing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unreadable bool
	}{{name: "hold ended"}, {name: "row unreadable", unreadable: true}} {
		t.Run(tc.name, func(t *testing.T) {
			var fs *seatReadFailingStore
			lc := newLadderLaneCityOver(t, func(s laneTestStore) beads.Store {
				fs = &seatReadFailingStore{snapshotWaitStore: &snapshotWaitStore{laneTestStore: s}}
				return fs
			}, "message")
			fs.seat = lc.seat
			id := lc.startedClaim()
			lc.at(0)
			lc.at(halfPast(1))
			op := lc.onlyOp()
			fs.on, fs.fail = true, tc.unreadable
			var log strings.Builder
			lc.lane.stderr = &log
			lc.at(2 * time.Minute)
			if got := lc.op(op.ID); got.Status != opjournal.StatusSubmitted || got.Cancel.Recorded() {
				t.Fatalf("op %s cancel %+v, want it left to the lane; log:\n%s", got.Status, got.Cancel, log.String())
			}
			if e := lc.entry(id); e.Notice.Op != op.ID || e.Attempts != 1 || e.Paused != 0 {
				t.Fatalf("entry %+v, want the mark pending", e)
			}
			if tc.unreadable && !strings.Contains(log.String(), "reading the row failed") {
				t.Fatalf("the failed read went unreported; log:\n%s", log.String())
			}
		})
	}
}

// A release that finds its claim handed to the seat (the live read of
// gc.session_id, stamped after the snapshot) starts a new episode: the
// unfinished-release nudge it shared a decision with is never proposed, and
// the started episode's first reminder comes after the grace, uncounted.
func TestWorkLadderReleaseFoundStartedProposesNoNotice(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	id := lc.claim("handed over after the snapshot")
	lc.lane.summary = func() *allocSummary {
		s := lc.summary()
		for i := range s.AssignedWork {
			s.AssignedWork[i].Metadata = maps.Clone(s.AssignedWork[i].Metadata)
			delete(s.AssignedWork[i].Metadata, beadmeta.SessionIDMetadataKey)
		}
		return s
	}
	lc.at(0)
	e := lc.entry(id)
	e.Attempts, e.Unreleased, e.Last = 3, true, lc.now.Add(-2*time.Hour)
	lc.sp.SetActivity(ladderSeatName, lc.now.Add(-3*time.Hour))
	lc.setRow(map[string]string{session.WorkLadderKey: workladder.EncodeLadder(workladder.Record{V: 1, Gen: 7, Entries: []workladder.Entry{e}})})
	if err := lc.work.SetMetadataBatch(id, map[string]string{beadmeta.SessionIDMetadataKey: lc.seat}); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	lc.lane.stderr = &log
	lc.at(time.Minute)
	if b, _ := lc.work.Get(id); b.Status != "in_progress" {
		t.Fatalf("claim %q: released though handed to the seat; log:\n%s", b.Status, log.String())
	}
	if ops := lc.ladderOps(); len(ops) != 0 || lc.nudges() != 0 {
		t.Fatalf("ops %+v, nudges %d: the old episode's reminder was sent", ops, lc.nudges())
	}
	got := lc.entry(id)
	if got.Rung != workladder.RungStarted || !got.Notice.IsZero() || got.Attempts != 0 || got.Paused != 0 || got.Refused != 0 {
		t.Fatalf("entry %+v, want a fresh started episode", got)
	}
	lc.at(2 * time.Minute)
	if got := lc.entry(id); got.Paused != 0 || got.Refused != 0 || len(lc.events(events.WorkNudgeFailed)) != 0 {
		t.Fatalf("entry %+v, nudge_failed %d: the old notice was re-counted on the new episode", got, len(lc.events(events.WorkNudgeFailed)))
	}
}

// A notice proposed and delivered at generation 7 whose fold lands on the
// pass that first sees generation 8 reports work.nudged at the notice's
// generation.
func TestWorkLadderNudgedEventCarriesTheNoticesGeneration(t *testing.T) {
	lc := newLadderLaneCity(t, "message")
	lc.startedClaim()
	lc.at(0)
	lc.at(halfPast(1))
	op := lc.onlyOp()
	lc.closeOp(op.ID, succeeded)
	lc.setRow(map[string]string{"generation": "8", "instance_token": "tok-8"})
	lc.at(2 * time.Minute)
	evs := lc.events(events.WorkNudged)
	if len(evs) != 1 {
		t.Fatalf("work.nudged = %d, want 1", len(evs))
	}
	var pl events.WorkLadderPayload
	if err := json.Unmarshal(evs[0].Payload, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Generation != 7 {
		t.Fatalf("work.nudged generation = %d, want the notice's 7", pl.Generation)
	}
}

// An open notice past its start_by is refused when no attempt may have
// reached the seat, and counts as failed when one may have.
func TestLadderNoticeFatePastStartBy(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	m := workladder.NoticeMark{Op: "gc-op-1", At: now.Add(-3 * time.Minute)}
	for maybe, want := range map[int]workladder.NoticeFate{0: workladder.NoticeRefused, 1: workladder.NoticeFailed} {
		op := opjournal.Op{ID: m.Op, Status: opjournal.StatusSubmitted, StartBy: now.Add(-time.Second), MaybeDelivered: maybe}
		got := ladderNoticeFate(op, nil, m, now)
		if got.pending || got.fate != want {
			t.Fatalf("maybe_delivered=%d: %+v, want fate %d", maybe, got, want)
		}
	}
}

// atBoth moves the ladder's and the lane fixture's clocks to d after the
// start and passes the ladder once, for a running lane host that reads the
// fixture's clock.
func (lc *ladderLaneCity) atBoth(d time.Duration) {
	lc.now = lc.start.Add(d)
	lc.f.clk.mu.Lock()
	lc.f.clk.now = lc.now
	lc.f.clk.mu.Unlock()
	lc.pass()
}

// fenceRuntime restarts the seat's fake runtime as incarnation 7 of tok-7,
// as a production start stamps it, so the lane's input fence matches, with
// its activity an hour back.
func (lc *ladderLaneCity) fenceRuntime() {
	lc.t.Helper()
	if err := lc.sp.Stop(ladderSeatName); err != nil {
		lc.t.Fatal(err)
	}
	env := map[string]string{runtime.RuntimeEpochEnv: "7", runtime.ContinuationEpochEnv: "1", runtime.InstanceTokenEnv: "tok-7"}
	if err := lc.sp.Start(context.Background(), ladderSeatName, runtime.Config{Env: env}); err != nil {
		lc.t.Fatal(err)
	}
	lc.sp.SetActivity(ladderSeatName, lc.start.Add(-time.Hour))
}

// The ladder's notice end to end in process: a running lane host's pass
// types it into the seat as one reminder block, closes it succeeded, and the
// next ladder pass settles the mark delivered (work.nudged, attempt 1).
func TestWorkLadderNoticeTypedByARunningLaneHost(t *testing.T) {
	lc := newLadderLaneCity(t)
	lc.fenceRuntime()
	hh := newLaneHostHarness(t, lc.f, "message")
	hh.start()
	hh.waitTicker(t)
	id := lc.startedClaim()
	lc.atBoth(0)
	lc.atBoth(halfPast(1))
	m := lc.entry(id).Notice
	if m.IsZero() {
		t.Fatalf("no notice mark: %+v (typed %d)", lc.entry(id), lc.nudges())
	}
	deadline := time.After(laneWaitBudget)
	for got := lc.op(m.Op); got.Status != opjournal.StatusClosed; got = lc.op(m.Op) {
		select {
		case <-deadline:
			t.Fatalf("the host never delivered %s: status %s, waiting %+v, logs %q", m.Op, got.Status, conversationLaneStatusFor(lc.f.dir, time.Now()).Waiting, hh.logLines())
		case <-hh.passes:
		case <-time.After(5 * time.Millisecond):
		}
	}
	if arm, reason := lc.op(m.Op).Outcome.Effective(); arm != opjournal.ArmSucceeded {
		t.Fatalf("outcome %s{%s}, want succeeded", arm, reason)
	}
	text := fmt.Sprintf(ladderStartedNudge, id)
	if ns := lc.f.nudges(); len(ns) != 1 || ns[0].Message != reminderGolden("planner", text) {
		t.Fatalf("typed %+v, want one reminder block carrying %q", ns, text)
	}
	lc.atBoth(2 * time.Minute)
	if n := len(lc.events(events.WorkNudged)); n != 1 {
		t.Fatalf("work.nudged = %d, want 1", n)
	}
	if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 1 || e.Refused != 0 {
		t.Fatalf("entry %+v, want the delivered attempt settled", e)
	}
}

// V2-18: a stop request landing after the proposal. A running lane host
// holds the notice (content_block stop_requested), the next ladder pass
// withdraws it, and nothing is typed into the stopping seat.
func TestWorkLadderNoticeHeldOnAStopRequestedRow(t *testing.T) {
	lc := newLadderLaneCity(t, "message") // registered, running no pass
	id := lc.startedClaim()
	lc.atBoth(0)
	lc.atBoth(halfPast(1))
	op := lc.onlyOp()
	lc.setRow(map[string]string{session.DrainIntentReasonKey: drainOrphaned, session.DrainIntentAtKey: lc.now.Format(time.RFC3339), session.DrainIntentIncarnationKey: "7"})
	hh := newLaneHostHarness(t, lc.f, "message") // replaces the registration with a running host
	hh.start()
	hh.waitTicker(t)
	hh.h.Poke(lc.seat)
	deadline := time.After(laneWaitBudget)
	for !laneStatusWaitsFor(conversationLaneStatusFor(lc.f.dir, time.Now()), op.ID, opjournal.WaitContentBlock, session.ContentGateStopRequested) {
		select {
		case <-deadline:
			t.Fatalf("the host never held the notice on stop_requested: %+v", conversationLaneStatusFor(lc.f.dir, time.Now()).Waiting)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if n := len(lc.f.nudges()); n != 0 {
		t.Fatalf("typed %d into a stop-requested seat", n)
	}
	lc.atBoth(2 * time.Minute)
	got := lc.op(op.ID)
	if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != ladderNoticePrincipal {
		t.Fatalf("op %s %+v cancel %+v, want withdrawn by the ladder", got.Status, got.Outcome, got.Cancel)
	}
	if n := len(lc.f.nudges()); n != 0 {
		t.Fatalf("typed %d into a stop-requested seat", n)
	}
	if e := lc.entry(id); !e.Notice.IsZero() || e.Attempts != 0 || e.Paused != 1 || e.Refused != 0 {
		t.Fatalf("entry %+v, want withdrawn: paused, no refusal", e)
	}
}
