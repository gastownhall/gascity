package runtime_test

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// staleShadowFake is shadowFake whose state cache still holds the live
// working record while the pane's own record is back at an input boundary:
// the turn ended inside the cache's TTL.
func staleShadowFake(t *testing.T) (*runtime.Fake, *[]runtime.ProgramStatusComparison) {
	t.Helper()
	f, got := shadowFake(t, true)
	stale := armedRow()
	stale.Mode = runtime.ProgramStatusShadow
	stale.Record.Since = time.Now()
	f.SetProgramCachedPane("s", stale)
	fresh := armedRow()
	fresh.Mode = runtime.ProgramStatusShadow
	fresh.Record.State, fresh.Record.Seq, fresh.Record.TurnEnds = runtime.ProgramIdle, 5, 1
	fresh.Record.Since, fresh.Record.QuietSince = time.Now(), time.Now()
	f.SetProgramPane("s", fresh)
	return f, got
}

// Kills an idle comparison outside the leaf (the op lane's and the CLI's
// pre-reads, wait_idle_preread) judged on the state cache's snapshot, which
// lags the record by up to its 2 s TTL (ga-f4ovjm.77): the record is read
// fresh, past the snapshot, and judged at the clock read after it.
func TestShadowCompareIdleJudgesTheFreshRecord(t *testing.T) {
	f, got := staleShadowFake(t)
	before := time.Now()
	runtime.ShadowCompareIdle(auto.New(runtime.FullFake{Fake: f}, runtime.NewFake()), "s", "gc-1", "claude", runtime.ProgramPointWaitIdlePreread, runtime.ProgramVerdictIdle)
	if len(*got) != 1 {
		t.Fatalf("recorded %+v, want one comparison", *got)
	}
	c := (*got)[0]
	if c.Program != runtime.ProgramVerdictIdle || c.Status.State != runtime.ProgramIdle {
		t.Fatalf("recorded program %s on a %q reading, want idle on the pane's own idle record, not the cache's working one", c.Program, c.Status.State)
	}
	if fresh := methodCalls(f, "ProgramStatusShadowRead"); fresh != 1 {
		t.Fatalf("%d fresh reads, want one", fresh)
	}
	if c.JudgedAt.Before(before) || c.Status.Since.After(c.JudgedAt) {
		t.Fatalf("judged at %v (call at %v, state entered %v): want the clock read after the reading", c.JudgedAt, before, c.Status.Since)
	}
}

// Kills an idle comparison outside the leaf that judges the record's hold
// bound on the record's age alone (observation judged lastActivity = 0, so a
// working record past ProgramStatusHoldLimit read indeterminate while the
// pane still drew the turn): the runtime's last activity keeps it live, and
// the comparison carries the activity it was judged with.
func TestShadowFreshReadingJudgesTheHoldWithActivity(t *testing.T) {
	f, got := shadowFake(t, true)
	row := armedRow()
	row.Mode = runtime.ProgramStatusShadow
	row.Record.Since = time.Now().Add(-runtime.ProgramStatusHoldLimit - time.Minute)
	f.SetProgramPane("s", row)
	activity := time.Now().Add(-time.Second)
	f.SetActivity("s", activity)
	runtime.ShadowCompareIdle(runtime.FullFake{Fake: f}, "s", "gc-1", "claude", runtime.ProgramPointWaitIdlePreread, runtime.ProgramVerdictBusy)
	if len(*got) != 1 || (*got)[0].Program != runtime.ProgramVerdictBusy || !(*got)[0].HoldActivity.Equal(activity) {
		t.Fatalf("recorded %+v, want busy judged with activity %v", *got, activity)
	}
}

// Kills a decision whose points each take their own read: a reading is
// taken once, by the first point that compares on it, and every point of
// the decision judges it.
func TestShadowReadingIsSharedByTheDecisionsPoints(t *testing.T) {
	f, got := shadowFake(t, true)
	r := runtime.NewShadowReading(runtime.FullFake{Fake: f}, "s")
	if fresh := methodCalls(f, "ProgramStatusShadowRead"); fresh != 0 {
		t.Fatalf("a reading no point compared on took %d reads", fresh)
	}
	now := time.Now()
	r.Compare("gc-1", "claude", runtime.ProgramPointQueuedDelivery, runtime.ProgramVerdictNoHold, now, runtime.ProgramHoldVerdict)
	r.Compare("gc-1", "claude", runtime.ProgramPointWaitIdlePreread, runtime.ProgramVerdictIdle, now, runtime.ProgramIdleVerdict)
	if fresh := methodCalls(f, "ProgramStatusShadowRead"); fresh != 1 {
		t.Fatalf("%d fresh reads for one decision, want one", fresh)
	}
	if len(*got) != 2 || (*got)[0].Program != runtime.ProgramVerdictHold || (*got)[1].Program != runtime.ProgramVerdictBusy ||
		(*got)[0].Status != (*got)[1].Status || !(*got)[0].JudgedAt.Equal((*got)[1].JudgedAt) {
		t.Fatalf("recorded %+v, want hold and busy on one reading", *got)
	}
	off, offGot := shadowFake(t, false)
	runtime.NewShadowReading(runtime.FullFake{Fake: off}, "s").Compare("gc-1", "claude", runtime.ProgramPointQueuedDelivery, runtime.ProgramVerdictNoHold, now, runtime.ProgramHoldVerdict)
	var none *runtime.ShadowReading
	none.Compare("gc-1", "claude", runtime.ProgramPointQueuedDelivery, runtime.ProgramVerdictNoHold, now, runtime.ProgramHoldVerdict)
	if len(*offGot) != 0 || methodCalls(off, "ProgramStatusShadowRead") != 0 {
		t.Fatalf("not shadowing recorded %+v", *offGot)
	}
}

// snapshotLeaf is a shadowing FullFake whose idle snapshot answers idle, err
// and counts its captures.
type snapshotLeaf struct {
	runtime.FullFake
	idle  bool
	err   error
	snaps int
}

func (l *snapshotLeaf) SnapshotIdle(string) (bool, error) {
	l.snaps++
	return l.idle, l.err
}

// Kills a wait-idle pre-read (wait_idle_preread) judged on the cached record
// (9 harms in the second shadow) or on a look of its own beside the
// pre-read's: the boundary is IdleBoundaryNow's, decided from one snapshot,
// and the record is read fresh right after that snapshot.
func TestShadowIdleBoundaryNowJudgesTheFreshRecordWithTheSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		idle   bool
		err    error
		scrape runtime.ProgramVerdict
		want   runtime.IdleBoundary
	}{
		{"idle", true, nil, runtime.ProgramVerdictIdle, runtime.IdleBoundaryAt},
		{"busy", false, nil, runtime.ProgramVerdictBusy, runtime.IdleBoundaryBusy},
		{"untrusted", false, runtime.ErrIdleIndeterminate, runtime.ProgramVerdictIndeterminate, runtime.IdleBoundaryUnknown},
		{"failed", false, errors.New("capture failed"), "", runtime.IdleBoundaryUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, got := staleShadowFake(t)
			leaf := &snapshotLeaf{FullFake: runtime.FullFake{Fake: f}, idle: c.idle, err: c.err}
			if b := runtime.ShadowIdleBoundaryNow(leaf, "s", "gc-1", "claude"); b != c.want {
				t.Fatalf("boundary %s, want %s", b, c.want)
			}
			if leaf.snaps != 1 {
				t.Fatalf("%d snapshots, want the pre-read's one", leaf.snaps)
			}
			if c.scrape == "" {
				if len(*got) != 0 {
					t.Fatalf("a failed snapshot recorded %+v", *got)
				}
				return
			}
			if len(*got) != 1 {
				t.Fatalf("recorded %+v, want one", *got)
			}
			cmp := (*got)[0]
			if cmp.Point != runtime.ProgramPointWaitIdlePreread || cmp.Scrape != c.scrape || cmp.Program != runtime.ProgramVerdictIdle ||
				cmp.SessionID != "gc-1" || cmp.Family != "claude" {
				t.Fatalf("recorded %+v, want wait_idle_preread %s/idle on the fresh record", cmp, c.scrape)
			}
		})
	}
	off, offGot := shadowFake(t, false)
	leaf := &snapshotLeaf{FullFake: runtime.FullFake{Fake: off}}
	if b := runtime.ShadowIdleBoundaryNow(leaf, "s", "gc-1", "claude"); b != runtime.IdleBoundaryBusy || len(*offGot) != 0 || methodCalls(off, "ProgramStatusShadowRead") != 0 {
		t.Fatalf("not shadowing: boundary %s, recorded %+v", b, *offGot)
	}
}

// Kills an observation (observation) judged on a reading the session view
// did not decide with (review of ga-f4ovjm.77: a fresh read, past the state
// cache's snapshot): under the on switch the view's activity and phase come
// from the reading it took, so a turn that ended inside the cache's TTL is
// served in-turn there, and judging the fresh idle record counted that as
// agreement. The record is judged on the view's own reading, the snapshot's
// working record, with the runtime's last activity for the hold bound, and
// no read of its own. A reading not taken under the shadow switch, or a
// leaf that is not shadowing, records nothing.
func TestShadowObserveIdle(t *testing.T) {
	f, got := staleShadowFake(t)
	stale := armedRow()
	stale.Mode = runtime.ProgramStatusShadow
	stale.Record.Since = time.Now().Add(-runtime.ProgramStatusHoldLimit - time.Minute)
	f.SetProgramCachedPane("s", stale)
	activity := time.Now().Add(-time.Second)
	f.SetActivity("s", activity)
	p := auto.New(runtime.FullFake{Fake: f}, runtime.NewFake())
	view, err := runtime.ReadProgramStatus(p, "s")
	if err != nil || view.State != runtime.ProgramWorking {
		t.Fatalf("the view read %+v, %v; want the snapshot's working record", view, err)
	}
	before := time.Now()
	runtime.ShadowObserveIdle(p, "s", "gc-1", "claude", runtime.ProgramVerdictIdle, view)
	if len(*got) != 1 {
		t.Fatalf("recorded %+v, want one comparison", *got)
	}
	c := (*got)[0]
	if c.Point != runtime.ProgramPointObservation || c.Scrape != runtime.ProgramVerdictIdle || c.Program != runtime.ProgramVerdictBusy ||
		c.Status != view || c.SessionID != "gc-1" || c.Family != "claude" || c.Session != "s" {
		t.Fatalf("recorded %+v, want observation idle/busy on the view's working reading for gc-1 (claude)", c)
	}
	if !c.HoldActivity.Equal(activity) || c.JudgedAt.Before(before) {
		t.Fatalf("judged with activity %v at %v, want %v at the clock read after it (call at %v)", c.HoldActivity, c.JudgedAt, activity, before)
	}
	if fresh, reads := methodCalls(f, "ProgramStatusShadowRead"), methodCalls(f, "ProgramStatus"); fresh != 0 || reads != 1 {
		t.Fatalf("%d fresh and %d cached reads, want the view's one cached read alone", fresh, reads)
	}

	on := view
	on.Mode = runtime.ProgramStatusOn
	runtime.ShadowObserveIdle(p, "s", "gc-1", "claude", runtime.ProgramVerdictIdle, on)
	if len(*got) != 1 {
		t.Fatalf("an on reading recorded %+v", (*got)[1:])
	}
	off, offGot := shadowFake(t, false)
	calls := len(off.SnapshotCalls())
	runtime.ShadowObserveIdle(runtime.FullFake{Fake: off}, "s", "gc-1", "claude", runtime.ProgramVerdictIdle, view)
	if len(*offGot) != 0 || len(off.SnapshotCalls()) != calls {
		t.Fatalf("a leaf that is not shadowing recorded %+v after calls %+v", *offGot, off.SnapshotCalls()[calls:])
	}
}
