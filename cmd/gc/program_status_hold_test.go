package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// heldTurnProvider is a liveTurnProvider (its screen's turn look scripted)
// whose agent pane also answers the program-status reads a FullFake
// answers: the cached reading, the shadow's fresh read, idle wait and sink,
// and the mid-turn hold's fresh read.
type heldTurnProvider struct {
	*liveTurnProvider
}

func (p heldTurnProvider) full() runtime.FullFake { return runtime.FullFake{Fake: p.Fake} }

func (p heldTurnProvider) ProgramStatus(name string) (runtime.ProgramStatus, error) {
	return p.full().ProgramStatus(name)
}

func (p heldTurnProvider) ProgramStatusShadowSink() runtime.ProgramStatusComparisonSink {
	return p.full().ProgramStatusShadowSink()
}

func (p heldTurnProvider) ProgramStatusShadowRead(name string) (runtime.ProgramStatus, bool) {
	return p.full().ProgramStatusShadowRead(name)
}

func (p heldTurnProvider) ProgramStatusShadowIdleWait(name string, started, decidedAt time.Time, scrape runtime.ProgramVerdict) {
	p.full().ProgramStatusShadowIdleWait(name, started, decidedAt, scrape)
}

func (p heldTurnProvider) ProgramStatusHoldRead(name string) (runtime.ProgramHoldReading, bool) {
	return p.full().ProgramStatusHoldRead(name)
}

// holdPaneRow is an armed agent-pane row under mode, declaring the mid-turn
// hold, whose record is in st, entered at since.
func holdPaneRow(mode runtime.ProgramStatusMode, st runtime.ProgramState, since time.Time) runtime.ProgramPaneRow {
	return runtime.ProgramPaneRow{
		Mode: mode, CaptureGate: true, PaneID: "%3", PanePipe: true, Tap: "r1.%3", HoldDeclared: true,
		TapLiveness: runtime.ProgramTapAlive, RecordForm: runtime.ProgramRecordDecoded,
		Record: runtime.ProgramStatus{State: st, Nonce: "r1.%3", Seq: 2, TurnStarts: 1, Progress: -1, Reported: true, Since: since},
	}
}

// holdBinding is a program_status binding declaring capture and, when hold
// is set, the mid-turn hold.
func holdBinding(hold bool) config.ProgramStatusBinding {
	b := config.ProgramStatusBinding{Protocol: config.ProgramStatusProtocolOSC7501, App: "agent"}
	if hold {
		b.HoldMidTurn = &hold
	}
	return b
}

// heldQueue is a busy session (its activity clock never quiet) holding one
// starved queued item, its screen showing no turn, its pane's record under
// mode in st, on a city switch city whose provider declares the hold when
// hold is set.
type heldQueue struct {
	busyQueuedNudgeFixture
	sp heldTurnProvider
}

func newHeldQueue(t *testing.T, mode runtime.ProgramStatusMode, st runtime.ProgramState, city string, hold bool) heldQueue {
	t.Helper()
	f := newBusyQueuedNudgeFixture(t)
	f.target.cfg = &config.City{Session: config.SessionConfig{ProgramStatus: city}}
	f.target.resolved = &config.ResolvedProvider{Name: "codex", ProgramStatus: holdBinding(hold)}
	f.fake.SetProgramPane(f.target.sessionName, holdPaneRow(mode, st, time.Now()))
	f.enqueue(t, newQueuedNudge("worker", "review the deploy logs", time.Now().Add(-10*time.Minute)))
	return heldQueue{busyQueuedNudgeFixture: f, sp: heldTurnProvider{liveTurnProvider: f.sp}}
}

// pass runs one poller pass, the quiescence gate closed.
func (q heldQueue) pass(t *testing.T) bool {
	t.Helper()
	delivered, err := tryDeliverQueuedNudgesByPoller(q.target, q.store, q.store, q.sp, time.Hour, q.obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	return delivered
}

// queue is the item's queue state: pending and in-flight counts and the
// attempts the pending items carry.
func (q heldQueue) queue(t *testing.T) (pending, inFlight, attempts int) {
	t.Helper()
	state, err := nudgequeue.LoadState(q.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range state.Pending {
		attempts += it.Attempts
	}
	return len(state.Pending), len(state.InFlight), attempts
}

// Kills a starved-queue bypass that types into a running turn of a program
// that aborts on mid-turn input (the Tier-4 cohort's 9 of 16 aborted long
// Codex turns): while the record says working, the screen's no-turn look
// is vetoed, nothing is claimed, sent or charged; once the record says the
// turn is done the next pass delivers it once, and no later pass sends it
// again.
func TestStarvedBypassVetoedWhileTheRecordSaysWorking(t *testing.T) {
	q := newHeldQueue(t, runtime.ProgramStatusOn, runtime.ProgramWorking, "on", true)
	for range 2 {
		if q.pass(t) || len(q.sends()) != 0 {
			t.Fatalf("a starved pass under a working record delivered: sends %+v", q.sends())
		}
		if p, f, a := q.queue(t); p != 1 || f != 0 || a != 0 {
			t.Fatalf("queue: pending %d in-flight %d attempts %d, want the item pending, unclaimed and uncharged", p, f, a)
		}
	}
	if n := q.fake.CountCalls("ProgramStatusHoldRead", q.target.sessionName); n != 2 {
		t.Fatalf("%d hold reads, want one per vetoed pass", n)
	}

	q.fake.SetProgramPane(q.target.sessionName, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramDone, time.Now()))
	if !q.pass(t) {
		t.Fatal("the pass after the turn ended did not deliver")
	}
	if sent := q.sends(); len(sent) != 1 || sent[0].Method != "NudgeNow" {
		t.Fatalf("sends %+v, want one immediate delivery", sent)
	}
	if p, f, _ := q.queue(t); p != 0 || f != 0 {
		t.Fatalf("queue after delivery: pending %d in-flight %d, want the item acked", p, f)
	}
	if q.pass(t) || len(q.sends()) != 1 {
		t.Fatalf("a later pass sent again: %+v", q.sends())
	}
}

// Kills a veto that misses a session naming no agent, as a provider
// session (API or dashboard kind=provider) names none: its target takes
// its provider's declaration by name, the declaration its pane was armed
// with, so the record vetoes its starved bypass as it does an agent
// session's, and the next pass after the turn delivers it.
func TestStarvedBypassVetoReachesASessionNamingNoAgent(t *testing.T) {
	q := newHeldQueue(t, runtime.ProgramStatusOn, runtime.ProgramWorking, "on", true)
	q.target = resolveNudgeTargetFromSessionInfo(q.dir, codexCatalogCity("on"), session.Info{
		ID: q.target.sessionID, SessionNameMetadata: q.target.sessionName, Template: "worker", Provider: "codex", SessionOrigin: "manual",
	})
	if q.pass(t) || len(q.sends()) != 0 {
		t.Fatalf("a starved pass under a working record delivered: sends %+v", q.sends())
	}
	if n := q.fake.CountCalls("ProgramStatusHoldRead", q.target.sessionName); n != 1 {
		t.Fatalf("%d hold reads, want the record read for the vetoed pass", n)
	}
	q.fake.SetProgramPane(q.target.sessionName, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramDone, time.Now()))
	if !q.pass(t) || len(q.sends()) != 1 {
		t.Fatalf("the pass after the turn ended: sends %+v, want one delivery", q.sends())
	}
}

// Kills a veto that also holds the bypass's own send look: the gate's
// record read passes, and a turn the record reports by the look right
// before the send holds the claimed delivery, handed back uncharged.
func TestStarvedBypassVetoedAtTheSendLook(t *testing.T) {
	q := newHeldQueue(t, runtime.ProgramStatusOn, runtime.ProgramIdle, "on", true)
	// The gate reads idle; by the read before the send a turn runs.
	reads := 0
	working := holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now())
	hook := heldTurnHook{heldTurnProvider: q.sp, before: func() {
		reads++
		if reads == 2 {
			q.fake.SetProgramPane(q.target.sessionName, working)
		}
	}}
	delivered, err := tryDeliverQueuedNudgesByPoller(q.target, q.store, q.store, hook, time.Hour, q.obs)
	if err != nil || delivered {
		t.Fatalf("delivered %t err %v, want the starved send held", delivered, err)
	}
	if len(q.sends()) != 0 || reads != 2 {
		t.Fatalf("sends %+v after %d hold reads, want none after two", q.sends(), reads)
	}
	if p, f, a := q.queue(t); p != 1 || f != 0 || a != 0 {
		t.Fatalf("queue: pending %d in-flight %d attempts %d, want the claim handed back uncharged", p, f, a)
	}
}

// heldTurnHook runs before ahead of each hold read.
type heldTurnHook struct {
	heldTurnProvider
	before func()
}

func (h heldTurnHook) ProgramStatusHoldRead(name string) (runtime.ProgramHoldReading, bool) {
	h.before()
	return h.heldTurnProvider.ProgramStatusHoldRead(name)
}

// Kills a veto that reaches past its scope: a provider that does not
// declare the hold, a pane that does not, and the switch at shadow or off
// each bypass as today, typing at once; a turn on the screen holds as
// today, with no record read; and only shadow records starved_bypass.
func TestStarvedBypassUnvetoedOutsideItsScope(t *testing.T) {
	for _, c := range []struct {
		name         string
		mode         runtime.ProgramStatusMode
		city         string
		providerHold bool
		paneHold     bool
		reads        int
	}{
		{"provider undeclared", runtime.ProgramStatusOn, "on", false, true, 0},
		{"pane undeclared", runtime.ProgramStatusOn, "on", true, false, 2}, // the gate's read and the send look's
		{"switch shadow", runtime.ProgramStatusShadow, "shadow", true, true, 0},
		{"switch off", runtime.ProgramStatusOff, "off", true, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := newHeldQueue(t, c.mode, runtime.ProgramWorking, c.city, c.providerHold)
			q.fake.SetProgramHold(q.target.sessionName, c.paneHold)
			var got []runtime.ProgramStatusComparison
			if c.mode == runtime.ProgramStatusShadow {
				q.fake.ProgramShadowSink = func(cmp runtime.ProgramStatusComparison) { got = append(got, cmp) }
			}
			if !q.pass(t) {
				t.Fatal("the starved pass did not deliver as today")
			}
			if sent := q.sends(); len(sent) != 1 || sent[0].Method != "NudgeNow" {
				t.Fatalf("sends %+v, want one immediate delivery as today", sent)
			}
			if n := q.fake.CountCalls("ProgramStatusHoldRead", q.target.sessionName); n != c.reads {
				t.Fatalf("%d hold reads, want %d", n, c.reads)
			}
			var bypass []runtime.ProgramStatusComparison
			for _, cmp := range got {
				if cmp.Point == runtime.ProgramPointStarvedBypass {
					bypass = append(bypass, cmp)
				}
			}
			if c.mode == runtime.ProgramStatusShadow {
				if len(bypass) == 0 || bypass[0].Scrape != runtime.ProgramVerdictNoHold || bypass[0].Program != runtime.ProgramVerdictHold {
					t.Fatalf("shadow comparisons %+v, want starved_bypass no_hold/hold", got)
				}
			} else if len(bypass) != 0 {
				t.Fatalf("starved_bypass recorded under %s: %+v", c.mode, bypass)
			}
		})
	}

	q := newHeldQueue(t, runtime.ProgramStatusOn, runtime.ProgramWorking, "on", true)
	q.sp.running = true
	if q.pass(t) || len(q.sends()) != 0 {
		t.Fatalf("a turn on screen: sends %+v, want held as today", q.sends())
	}
	if n := q.fake.CountCalls("ProgramStatusHoldRead", q.target.sessionName); n != 0 {
		t.Fatalf("a turn on screen took %d hold reads, want none", n)
	}
}

// Kills a hold on the normal path: a record that says working while the
// pane is quiet (Codex reports working while a background terminal runs,
// with no turn) delivers through the quiescence gate exactly as the switch
// off does, with no record read.
func TestNormalPathDeliversAsOffWhateverTheRecord(t *testing.T) {
	run := func(mode runtime.ProgramStatusMode, city string) ([]runtime.Call, int) {
		q := newHeldQueue(t, mode, runtime.ProgramWorking, city, true)
		quiet := time.Now().Add(-time.Hour)
		q.fake.Activity = map[string]time.Time{q.target.sessionName: quiet}
		q.obs.LastActivity = &quiet
		delivered, err := tryDeliverQueuedNudgesByPoller(q.target, q.store, q.store, q.sp, time.Second, q.obs)
		if err != nil || !delivered {
			t.Fatalf("%s: delivered %t err %v, want the quiet session delivered", mode, delivered, err)
		}
		var methods []runtime.Call
		for _, c := range q.sends() {
			methods = append(methods, runtime.Call{Method: c.Method, Message: c.Message})
		}
		return methods, q.fake.CountCalls("ProgramStatusHoldRead", q.target.sessionName)
	}
	off, _ := run(runtime.ProgramStatusOff, "off")
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
		got, reads := run(mode, string(mode))
		if fmt.Sprint(got) != fmt.Sprint(off) || reads != 0 {
			t.Fatalf("%s: sends %+v after %d hold reads, want off's %+v and none", mode, got, reads, off)
		}
	}
}

// Kills a work ladder that counts a delivery the runtime deferred (a turn
// its keys would interrupt is running, nothing typed) toward a recycle, or
// raises nudge_refused for it: the attempt is re-counted as paused, with no
// refusal.
func TestWorkLadderBusyDeferralIsUncounted(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("any")
	c.sp.NudgeErrors = map[string]error{ladderSeatName: fmt.Errorf("turn running: %w", runtime.ErrSessionBusy)}
	c.now = c.now.Add(-2 * time.Minute)
	c.pass()
	c.now = c.now.Add(2 * time.Minute)
	c.pass()
	if e := c.entry(id); e.Attempts != 0 || e.Paused != 1 || e.Refused != 0 {
		t.Fatalf("entry %+v, want the attempt re-counted as paused with no refusal", e)
	}
	c.run(6*time.Hour, 30*time.Second, nil)
	if got := escalations(c); len(got) != 0 {
		t.Fatalf("escalations %v for busy deferrals, want none", got)
	}
	if e := c.entry(id); e.Attempts != 0 || e.Refused != 0 || len(c.posted) != 0 {
		t.Fatalf("entry %+v, recycles %+v: deferrals moved it toward a recycle", e, c.posted)
	}
}
