package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// holdLeaf is a running fake session "s", quiet for an hour, whose agent
// pane reads a record in st entered at since under mode, and declares the
// mid-turn hold when declared.
func holdLeaf(t *testing.T, mode runtime.ProgramStatusMode, st runtime.ProgramState, since time.Time, declared bool) (*runtime.Fake, runtime.FullFake) {
	t.Helper()
	f := runtime.NewFake()
	if err := f.Start(context.Background(), "s", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	f.Activity = map[string]time.Time{"s": time.Now().Add(-time.Hour)}
	f.WaitForIdleErrors = map[string]error{"s": nil}
	row := holdPaneRow(mode, st, since)
	row.HoldDeclared = declared
	f.SetProgramPane("s", row)
	return f, runtime.FullFake{Fake: f}
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

// holdTarget is a nudge target for session "s" under the city switch mode,
// on a provider whose program_status declares capture and, when hold is
// set, the mid-turn hold.
func holdTarget(mode string, hold bool) nudgeTarget {
	binding := config.ProgramStatusBinding{Protocol: config.ProgramStatusProtocolOSC7501, App: "agent"}
	if hold {
		binding.HoldMidTurn = &hold
	}
	return nudgeTarget{
		sessionName: "s",
		sessionID:   "gc-1",
		cfg:         &config.City{Session: config.SessionConfig{ProgramStatus: mode}},
		resolved:    &config.ResolvedProvider{Name: "agent", ProgramStatus: binding},
	}
}

// quietObs is an observation an hour quiet, which the quiescence rule
// delivers to.
func quietObs() worker.LiveObservation {
	stale := time.Now().Add(-time.Hour)
	return worker.LiveObservation{LastActivity: &stale}
}

// holdReads counts the fresh hold reads f took.
func holdReads(f *runtime.Fake) int {
	n := 0
	for _, c := range f.Calls {
		if c.Method == "ProgramStatusHoldRead" {
			n++
		}
	}
	return n
}

// Kills a queued delivery that is claimed into a running turn of a program
// that aborts on mid-turn input: under on, a fresh live working or blocked
// record closes the gate a quiet session would open, before any starvation
// bypass, and holds the conversation lane's idle reading too.
func TestQueuedDeliveryHeldWhileTheRecordSaysWorking(t *testing.T) {
	for _, st := range []runtime.ProgramState{runtime.ProgramWorking, runtime.ProgramBlocked} {
		t.Run(string(st), func(t *testing.T) {
			f, sp := holdLeaf(t, runtime.ProgramStatusOn, st, time.Now(), true)
			target := holdTarget("on", true)
			gate, err := queuedNudgeGateFor(target, sp, time.Second, quietObs(), time.Now())
			if err != nil || gate.open {
				t.Fatalf("gate = %+v, %v; want closed with no error", gate, err)
			}
			if pollerSessionIdleEnough(target, sp, time.Second, quietObs()) {
				t.Fatal("pollerSessionIdleEnough read a held session idle enough")
			}
			if holdReads(f) != 2 {
				t.Fatalf("%d hold reads, want one per decision", holdReads(f))
			}
		})
	}
}

// Kills a hold that trusts the state cache's snapshot: the snapshot still
// says working, the pane's record already says idle, and the delivery goes.
func TestQueuedDeliveryReleasedByAFreshIdleRecord(t *testing.T) {
	f, sp := holdLeaf(t, runtime.ProgramStatusOn, runtime.ProgramIdle, time.Now(), true)
	f.SetProgramCachedPane("s", holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now()))
	gate, err := queuedNudgeGateFor(holdTarget("on", true), sp, time.Second, quietObs(), time.Now())
	if err != nil || !gate.open {
		t.Fatalf("gate = %+v, %v; want open on the fresh idle record", gate, err)
	}
	if holdReads(f) != 1 {
		t.Fatalf("%d hold reads, want one", holdReads(f))
	}
}

// Kills a hold that outlives its bound: a working record no activity
// refreshed for longer than the hold limit holds nothing.
func TestQueuedDeliveryHoldIsBounded(t *testing.T) {
	_, sp := holdLeaf(t, runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now().Add(-runtime.ProgramStatusHoldLimit-time.Minute), true)
	gate, err := queuedNudgeGateFor(holdTarget("on", true), sp, time.Second, quietObs(), time.Now())
	if err != nil || !gate.open {
		t.Fatalf("gate = %+v, %v; want open past the hold bound", gate, err)
	}
}

// Kills a hold that reaches a provider that does not declare it, a pane
// that does not, or a switch that is not on: each delivers as today and,
// but for a declared pane under on, takes no hold read at all.
func TestQueuedDeliveryUnheldWithoutTheDeclarationOrSwitch(t *testing.T) {
	for _, c := range []struct {
		name         string
		mode         runtime.ProgramStatusMode
		city         string
		providerHold bool
		paneHold     bool
		reads        int
	}{
		{"provider undeclared", runtime.ProgramStatusOn, "on", false, true, 0},
		{"pane undeclared", runtime.ProgramStatusOn, "on", true, false, 1},
		{"switch shadow", runtime.ProgramStatusShadow, "shadow", true, true, 0},
		{"switch off", runtime.ProgramStatusOff, "off", true, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, sp := holdLeaf(t, c.mode, runtime.ProgramWorking, time.Now(), c.paneHold)
			gate, err := queuedNudgeGateFor(holdTarget(c.city, c.providerHold), sp, time.Second, quietObs(), time.Now())
			if err != nil || !gate.open {
				t.Fatalf("gate = %+v, %v; want open as today", gate, err)
			}
			if holdReads(f) != c.reads {
				t.Fatalf("%d hold reads, want %d", holdReads(f), c.reads)
			}
		})
	}
}

// heldRecordRow is holdPaneRow under on, working since now: a live hold.
func heldRecordRow() runtime.ProgramPaneRow {
	return holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now())
}

// Kills an execution backstop that reserves an attempt (and in time drains
// the seat) for a seat every other gate reads quiet while its program's own
// record says a turn is running: it holds under the record's own reason,
// writing no pacing state.
func TestExecutionBackstopHoldsOnAFreshProgramRecord(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.provider = runtime.FullFake{Fake: f.sp}

	// As f.tickStale, with the turn still drawing: its record entered
	// working a minute before each tick's clock, so the hold stays live.
	for i := 0; i < idleClaimNudgeMaxAttempts+3; i++ {
		f.idleFor(t, 10*time.Minute)
		judged := f.now
		if now := time.Now(); now.After(judged) {
			judged = now
		}
		f.sp.SetProgramPane(f.sessName, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, judged.Add(-time.Minute)))
		f.tick(t)
		f.now = f.now.Add(idleClaimNudgeGrace + idleClaimNudgeBackoff)
	}

	if got := f.nudgeCount(); got != 0 {
		t.Fatalf("nudges to a seat its record holds = %d, want 0; stdout=%s", got, f.stdout.String())
	}
	if got := f.sessionMeta(t, executionClaimNudgeWorkKey); got != "" {
		t.Fatalf("persisted work marker = %q, want no attempt reserved", got)
	}
	if len(f.drained) != 0 {
		t.Fatalf("drain requests = %v, want none", f.drained)
	}
	if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldProgramRecord+" ") {
		t.Fatalf("hold breadcrumb = %q, want %s", got, executionHoldProgramRecord)
	}
}

// Kills a claim-nudge hold that lets a seat its record holds be nudged.
func TestClaimNudgeHoldsOnAFreshProgramRecord(t *testing.T) {
	_, sp := holdLeaf(t, runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now(), true)
	row := beads.Bead{ID: "gc-1", Metadata: map[string]string{"provider": "codex", "session_name": "s"}}
	if got := claimNudgeBusyHold(sp, row, "s", time.Now()); got != executionHoldProgramRecord {
		t.Fatalf("claimNudgeBusyHold = %q, want %s", got, executionHoldProgramRecord)
	}
	_, idle := holdLeaf(t, runtime.ProgramStatusOn, runtime.ProgramIdle, time.Now(), true)
	if got := claimNudgeBusyHold(idle, row, "s", time.Now()); got != "" {
		t.Fatalf("claimNudgeBusyHold at an idle record = %q, want quiet", got)
	}
}

// Kills a work ladder whose direct path counts an attempt for a seat whose
// program's record says a turn is running: no nudge is sent, and none is
// counted.
func TestWorkLadderHoldsOnAFreshProgramRecord(t *testing.T) {
	c := newLadderCity(t)
	id := c.claim("a long turn")
	c.sp.SetProgramPane(ladderSeatName, heldRecordRow())
	c.env.SP = runtime.FullFake{Fake: c.sp}
	c.run(30*time.Minute, 30*time.Second, nil)
	if len(c.nudgedAt) != 0 || c.sp.CountCalls("Nudge", ladderSeatName) != 0 {
		t.Fatalf("nudges %v while the record holds the seat", c.nudgedAt)
	}
	if e := c.entry(id); e.Attempts != 0 {
		t.Fatalf("entry %+v, want no attempt counted", e)
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

// Kills a drain reminder that writes its budget marker for a session whose
// program's record says a turn is running: it holds as busy, nothing
// typed, nothing spent, and delivers once the record is idle.
func TestDrainReminderHoldsOnAFreshProgramRecord(t *testing.T) {
	e := newDrainReminderEnv(t)
	e.sp.SetProgramPane(e.name, heldRecordRow())
	sp := runtime.FullFake{Fake: e.sp}
	if got := e.remindWith(sp); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held", got)
	}
	if n := len(e.nudges()); n != 0 {
		t.Errorf("nudge count = %d, want 0", n)
	}
	if got := e.meta(drainReminderHoldKey); got != drainReminderHoldBusy {
		t.Errorf("%s = %q, want %q", drainReminderHoldKey, got, drainReminderHoldBusy)
	}
	if got := e.meta(drainReminderCountKey); got != "" {
		t.Errorf("%s = %q, want unset: a held reminder spends nothing", drainReminderCountKey, got)
	}
	e.sp.SetProgramPane(e.name, holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramIdle, time.Now()))
	if got := e.remindWith(sp); got != drainReminderDelivered {
		t.Fatalf("outcome at an idle record = %v, want delivered", got)
	}
}

// Kills a queue that spends an attempt on, or delivers into, a running turn
// of a program that aborts on mid-turn input, or delivers its nudge twice:
// the poller's gate holds the queued item unclaimed while the record says
// working; a pass whose target does not resolve the hold claims it, the
// leaf refuses it as busy, and the claim is released uncharged; once the
// record says idle the next pass delivers it, acks it, and no later pass
// sends it again.
func TestQueuedNudgeHeldThenDeliveredOnceAfterTheTurnEnds(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "check hook output", time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	fake := runtime.NewFake()
	sp := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: fake})
	if err := sp.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{"sess-worker": idleSince}
	fake.SetProgramPane("sess-worker", holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramWorking, time.Now()))
	target := holdTarget("on", true)
	target.cityPath, target.sessionName, target.agent = dir, "sess-worker", config.Agent{Name: "worker"}
	undeclared := target
	undeclared.resolved = &config.ResolvedProvider{Name: "agent"}
	obs := worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince}

	pass := func(target nudgeTarget) bool {
		t.Helper()
		store := openNudgeBeadStore(dir).Store
		delivered, err := tryDeliverQueuedNudgesByPoller(target, store, store, sp, 3*time.Second, obs)
		if err != nil {
			t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
		}
		return delivered
	}
	queue := func() (pending, inFlight, attempts int) {
		t.Helper()
		if err := withNudgeQueueState(dir, "test", func(state *nudgeQueueState) error {
			pending, inFlight = len(state.Pending), len(state.InFlight)
			for _, it := range state.Pending {
				attempts += it.Attempts
			}
			for i := range state.Pending {
				state.Pending[i].DeliverAfter = time.Now().Add(-time.Second)
			}
			return nil
		}); err != nil {
			t.Fatalf("withNudgeQueueState: %v", err)
		}
		return pending, inFlight, attempts
	}
	nudges := func() int {
		return fake.CountCalls("Nudge", "sess-worker") + fake.CountCalls("NudgeNow", "sess-worker")
	}

	if pass(target) || nudges() != 0 {
		t.Fatalf("a pass under a working record delivered (nudges %d)", nudges())
	}
	if p, f, a := queue(); p != 1 || f != 0 || a != 0 {
		t.Fatalf("queue after the held pass: pending %d in-flight %d attempts %d, want the item pending, unclaimed and uncharged", p, f, a)
	}

	if pass(undeclared) || nudges() != 1 {
		t.Fatalf("a pass claiming past the gate: nudges %d, want one refused delivery", nudges())
	}
	if p, f, a := queue(); p != 1 || f != 0 || a != 0 {
		t.Fatalf("queue after the leaf's refusal: pending %d in-flight %d attempts %d, want the claim released uncharged", p, f, a)
	}

	fake.SetProgramPane("sess-worker", holdPaneRow(runtime.ProgramStatusOn, runtime.ProgramDone, time.Now()))
	if !pass(target) || nudges() != 2 {
		t.Fatalf("the pass after the turn ended did not deliver (nudges %d)", nudges())
	}
	if p, f, _ := queue(); p != 0 || f != 0 {
		t.Fatalf("queue after delivery: pending %d in-flight %d, want the item acked", p, f)
	}
	if pass(target) || nudges() != 2 {
		t.Fatalf("a later pass delivered again (nudges %d), want exactly once", nudges())
	}
}
