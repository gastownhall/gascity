package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// freshReads counts f's fresh shadow reads.
func freshReads(f *runtime.Fake) int {
	n := 0
	for _, c := range f.Calls {
		if c.Method == "ProgramStatusShadowRead" {
			n++
		}
	}
	return n
}

// Kills a queued delivery's record hold (queued_delivery) judged on the
// state cache's snapshot (ga-f4ovjm.77: 17 stale readings in the second
// shadow): a turn that ended inside the cache's TTL still held there. The
// record is read fresh with the decision, which stays today's.
func TestQueuedDeliveryJudgesTheFreshRecord(t *testing.T) {
	stale := time.Now().Add(-time.Hour)
	decide := func(mode runtime.ProgramStatusMode) (bool, []runtime.ProgramStatusComparison) {
		sp, got := shadowLeaf(t, mode)
		cached, _ := sp.ProgramPane("s")
		sp.SetProgramCachedPane("s", cached)
		shadowTurnEnded(sp.Fake, "s")
		deliver := pollerSessionIdleEnough(nudgeTarget{sessionName: "s", sessionID: "gc-1"}, sp, time.Second, worker.LiveObservation{LastActivity: &stale})
		return deliver, got()
	}
	offDeliver, offGot := decide(runtime.ProgramStatusOff)
	deliver, got := decide(runtime.ProgramStatusShadow)
	if deliver != offDeliver || !deliver {
		t.Fatalf("shadow decided %v, off %v; want the quiet session delivered both ways", deliver, offDeliver)
	}
	if len(offGot) != 0 {
		t.Fatalf("the switch off recorded %+v", offGot)
	}
	if len(got) != 1 || got[0].Point != runtime.ProgramPointQueuedDelivery || got[0].Scrape != runtime.ProgramVerdictNoHold ||
		got[0].Program != runtime.ProgramVerdictNoHold || got[0].Status.State != runtime.ProgramIdle {
		t.Fatalf("recorded %+v, want queued_delivery no_hold/no_hold on the pane's own idle record", got)
	}
}

// Kills an op-lane look whose idle gate (queued_delivery) and screen
// pre-read (wait_idle_preread) judge the state cache's snapshot, or take a
// fresh read each: they are one decision, judged on one fresh read. A host
// re-probing a busy runtime on its backoff reuses the pass's executor, and
// each re-probe is a look of its own: a reading kept from an earlier look
// would judge its points on a record the program has since left.
func TestLaneLookSharesOneFreshRead(t *testing.T) {
	f, deps, _ := newScreenTurnFixture(t, "claude")
	got := shadowWorking(f.sp, f.name, true)
	cached, _ := f.sp.ProgramPane(f.name)
	f.sp.SetProgramCachedPane(f.name, cached)
	shadowTurnEnded(f.sp, f.name)
	f.showTurn(false)
	f.sp.SetActivity(f.name, time.Now().Add(-time.Hour))
	v, row := f.view(t)
	e := newConversationLaneExec(deps, row)
	before := freshReads(f.sp)
	p, err := e.Probe(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if p.Boundary != opjournal.TriYes {
		t.Fatalf("boundary %v, want the quiet idle screen's yes", p.Boundary)
	}
	if reads := freshReads(f.sp) - before; reads != 1 {
		t.Fatalf("the look took %d fresh reads, want one", reads)
	}
	all := got()
	gate, pre := atPoint(all, runtime.ProgramPointQueuedDelivery), atPoint(all, runtime.ProgramPointWaitIdlePreread)
	if len(gate) != 1 || len(pre) != 1 {
		t.Fatalf("recorded %+v, want one queued_delivery and one wait_idle_preread", all)
	}
	if gate[0].Program != runtime.ProgramVerdictNoHold || pre[0].Program != runtime.ProgramVerdictIdle ||
		gate[0].Status != pre[0].Status || !gate[0].HoldActivity.Equal(pre[0].HoldActivity) {
		t.Fatalf("idle gate %+v and pre-read %+v, want no_hold and idle on one fresh idle reading", gate[0], pre[0])
	}

	pane, _ := f.sp.ProgramPane(f.name)
	pane.Record.State, pane.Record.Seq, pane.Record.TurnStarts, pane.Record.Since = runtime.ProgramWorking, 4, 2, time.Now()
	f.sp.SetProgramPane(f.name, pane)
	before = freshReads(f.sp)
	if _, err := e.Reprobe(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if reads := freshReads(f.sp) - before; reads != 1 {
		t.Fatalf("the re-probe's look took %d fresh reads, want one of its own", reads)
	}
	again := got()[len(all):]
	gate, pre = atPoint(again, runtime.ProgramPointQueuedDelivery), atPoint(again, runtime.ProgramPointWaitIdlePreread)
	if len(gate) != 1 || len(pre) != 1 || gate[0].Status.State != runtime.ProgramWorking || gate[0].Status.Seq != 4 ||
		gate[0].Program != runtime.ProgramVerdictHold || pre[0].Status != gate[0].Status || pre[0].Program != runtime.ProgramVerdictBusy {
		t.Fatalf("re-probe recorded %+v, want its idle gate (hold) and pre-read (busy) on its own fresh reading of the next turn's working record", again)
	}
}

// Kills an op-lane delivery whose idle wait (wait_for_idle) is judged after
// the delivery reached the runtime (review of ga-f4ovjm.77: the wait was
// deferred to the leaf nudge's read before its paste, which comes after the
// nudge lock and gc's own keystrokes): a reading that late judges what came
// between, the turn the delivery starts included. The wait is recorded at
// its decision, before the nudge.
func TestLaneRecordsItsIdleWaitBeforeItDelivers(t *testing.T) {
	f, deps, st := newScreenTurnFixture(t, "claude")
	shadowWorking(f.sp, f.name, true)
	nudgesAtWait := []int{}
	f.sp.ProgramShadowSink = func(c runtime.ProgramStatusComparison) {
		if c.Point == runtime.ProgramPointWaitForIdle {
			nudgesAtWait = append(nudgesAtWait, f.sp.CountCalls("Nudge", f.name))
		}
	}
	f.showTurn(false)
	f.sp.SetActivity(f.name, f.clk.Now().Add(-3400*time.Millisecond))
	l := f.lane(t, st)
	f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "please run the tests"))
	v, row := f.view(t)
	e := newConversationLaneExec(deps, row)
	quiescenceOnFixtureClock(f, e)
	if _, err := l.Pass(context.Background(), v, e); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if n := len(f.nudges()); n != 1 {
		t.Fatalf("delivered %d nudges, want the boundary delivery's one", n)
	}
	if len(nudgesAtWait) != 1 || nudgesAtWait[0] != 0 {
		t.Fatalf("idle waits recorded with %v nudges already delivered, want one recorded before the delivery", nudgesAtWait)
	}
}
