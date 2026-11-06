package tmux

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// decidedStartOps are start ops whose startup ladder decides with tm's
// program record (Tmux.programStartupLadder) and hand a held-back nudge to
// tm, as tmuxStartOps do.
type decidedStartOps struct {
	*fakeStartOps
	tm *Tmux
}

func (o decidedStartOps) programStartupLadder(name string, send func() error, busy func() (bool, error)) (func() error, func() (bool, error)) {
	return o.tm.programStartupLadder(name, send, busy)
}

func (o decidedStartOps) handBackStartupNudge(name, nudge string) { o.tm.handbacks.set(name, nudge) }

// fastStartupLadder shrinks the startup ladder's backoffs for the test.
func fastStartupLadder(t *testing.T) {
	t.Helper()
	orig := startupNudgeRetryBackoffs
	startupNudgeRetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { startupNudgeRetryBackoffs = orig })
}

// startupRun is one start of session "s" under mode with sends answering
// sends in turn and the nth fresh read answering rows(n), and what it did.
type startupRun struct {
	err      error
	ops      *fakeStartOps
	reads    int
	handback string
	handed   bool
}

func runStartupLadder(t *testing.T, mode runtime.ProgramStatusMode, sends []error, rows func(n int) string) startupRun {
	t.Helper()
	fastStartupLadder(t)
	tm, ex := holdTmux(mode, &fakeExecutor{}, rows)
	ops := &fakeStartOps{hasSessionResult: true, sendKeysErrs: slices.Clone(sends)}
	cfg := runtime.Config{Command: "claude", Nudge: "start working"}
	err := doStartSession(context.Background(), decidedStartOps{ops, tm}, "s", cfg, DefaultConfig().SetupTimeout)
	nudge, handed := tm.handbacks.take("s")
	return startupRun{err: err, ops: ops, reads: ex.holdReads(), handback: nudge, handed: handed}
}

// count is how many times the run's ops were called as method.
func (r startupRun) count(method string) int {
	n := 0
	for _, m := range r.ops.callMethods() {
		if m == method {
			n++
		}
	}
	return n
}

// startupRows answers the baseline read (n == 0) with base and every later
// read with later, each a hold read row of agent pane %1 under holdNonce.
func startupRows(base, later string) func(int) string {
	return func(n int) string {
		if n == 0 {
			return holdPaneRow(base, "", 0)
		}
		return holdPaneRow(later, "", 0)
	}
}

// Kills a ladder that ignores the program's turn start past its baseline
// (record says confirmed, scan says unconfirmed): under on the nudge is
// confirmed at once and never re-sent; under off and shadow the scan's
// ladder re-sends as before and reads no record.
func TestStartupLadderConfirmsOnATurnStartPastTheBaseline(t *testing.T) {
	idle := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince)
	started := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 2, 2 })
	sends := []error{ErrNudgeSubmitUnconfirmed, nil}

	on := runStartupLadder(t, runtime.ProgramStatusOn, sends, startupRows(idle, started))
	if on.err != nil || on.count("sendKeys") != 1 || on.count("recordUnconfirmedNudge") != 0 {
		t.Fatalf("on: err %v, calls %v; want one send confirmed by the record", on.err, on.ops.callMethods())
	}
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow} {
		got := runStartupLadder(t, mode, sends, startupRows(idle, started))
		if got.err != nil || got.count("sendKeys") != 2 || got.reads != 0 {
			t.Fatalf("%s: err %v, calls %v, %d reads; want the scan's ladder (two sends) and no read", mode, got.err, got.ops.callMethods(), got.reads)
		}
	}
}

// Kills a confirmation credited without positive evidence: a record still
// at the baseline, a baseline that was itself busy (a startup turn already
// running), and a record under another nonce credit nothing, so the scan's
// ladder decides.
func TestStartupLadderCreditsOnlyPositiveEvidence(t *testing.T) {
	idle := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince)
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince)
	later := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 5, 3 })
	for _, c := range []struct {
		name        string
		base, later string
	}{
		{"record unchanged", idle, idle},
		{"busy baseline", working, later},
		{"silent baseline", psRecord(t, holdNonce, ""), later},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := runStartupLadder(t, runtime.ProgramStatusOn, []error{ErrNudgeSubmitUnconfirmed, nil}, startupRows(c.base, c.later))
			if got.err != nil || got.count("sendKeys") != 2 {
				t.Fatalf("err %v, calls %v; want the scan's ladder to re-send", got.err, got.ops.callMethods())
			}
		})
	}
}

// Kills a ladder that types into a dialog the program reports, or fails the
// start on it (design §10.2 row A20): under on a live blocked record holds
// every send (nothing is typed), the ladder backs off as for a deferral, and
// at its end the start succeeds with the unconfirmed nudge recorded and
// handed back for its caller to queue. Under off and shadow the same pane
// is typed into as before.
func TestStartupNudgeTreatsBlockedAsBusyAndNeverFailsStart(t *testing.T) {
	idle := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince)
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })

	on := runStartupLadder(t, runtime.ProgramStatusOn, []error{ErrNudgeSubmitUnconfirmed}, startupRows(idle, blocked))
	if on.err != nil {
		t.Fatalf("on: start failed %v; a dialog never fails a start", on.err)
	}
	if n := on.count("sendKeys"); n != 0 {
		t.Fatalf("on: %d sends into a program-reported dialog; want none (calls %v)", n, on.ops.callMethods())
	}
	if on.count("recordUnconfirmedNudge") != 1 || !on.handed || on.handback != "start working" {
		t.Fatalf("on: calls %v, handback %q (%v); want the nudge recorded and handed back", on.ops.callMethods(), on.handback, on.handed)
	}
	if want := 1 + 1 + len(startupNudgeRetryBackoffs); on.reads != want {
		t.Fatalf("on: %d record reads; want the baseline and one before each of %d sends", on.reads, want-1)
	}
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow} {
		got := runStartupLadder(t, mode, []error{nil}, startupRows(idle, blocked))
		if got.err != nil || got.count("sendKeys") != 1 || got.handed || got.reads != 0 {
			t.Fatalf("%s: err %v, calls %v, handed %v, %d reads; want the send typed as before", mode, got.err, got.ops.callMethods(), got.handed, got.reads)
		}
	}
}

// Kills a blocked hold that outlives the dialog, or one taken from a stale
// record: a dialog that closes before the ladder ends lets the next send
// go, and a blocked record past its hold bound holds nothing.
func TestStartupLadderBlockedHoldIsLiveOnly(t *testing.T) {
	idle := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince)
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })
	answered := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 3 })
	t.Run("dialog answered", func(t *testing.T) {
		rows := func(n int) string {
			switch {
			case n == 0:
				return holdPaneRow(idle, "", 0)
			case n <= 2:
				return holdPaneRow(blocked, "", 0)
			}
			return holdPaneRow(answered, "", 0)
		}
		got := runStartupLadder(t, runtime.ProgramStatusOn, nil, rows)
		if got.err != nil || got.count("sendKeys") != 1 || got.handed {
			t.Fatalf("err %v, calls %v, handed %v; want one send once the dialog closed", got.err, got.ops.callMethods(), got.handed)
		}
	})
	t.Run("blocked past the hold bound", func(t *testing.T) {
		stale := psRecord(t, holdNonce, runtime.ProgramBlocked, func(s *runtime.ProgramStatus) {
			s.Seq, s.Since = 2, time.Now().Add(-runtime.ProgramStatusHoldLimit-time.Minute)
		})
		got := runStartupLadder(t, runtime.ProgramStatusOn, nil, startupRows(idle, stale))
		if got.err != nil || got.count("sendKeys") != 1 || got.handed {
			t.Fatalf("err %v, calls %v, handed %v; want the stale record to hold nothing", got.err, got.ops.callMethods(), got.handed)
		}
	})
}

// Kills a provider whose next Start inherits an untaken handback.
func TestStartClearsAnUntakenStartupHandback(t *testing.T) {
	var h startupHandbacks
	h.set("s", "old nudge")
	h.set("s", "")
	if nudge, ok := h.take("s"); ok {
		t.Fatalf("took %q after a clear; want nothing", nudge)
	}
	h.set("s", "nudge")
	if nudge, ok := h.take("s"); !ok || nudge != "nudge" {
		t.Fatalf("take = %q, %v; want the nudge", nudge, ok)
	}
	if _, ok := h.take("s"); ok {
		t.Fatal("a handback taken twice")
	}
}

// settleTmux is a Tmux under mode whose scan always reads busy and whose nth
// fresh read answers rows(n), with an interrupt of session "s" noted on the
// first read.
func settleTmux(t *testing.T, mode runtime.ProgramStatusMode, rows func(n int) string) (*Tmux, *holdExecutor) {
	t.Helper()
	tm, ex := holdTmux(mode, &paneScriptExecutor{captures: []scriptedCapture{{out: busyPane}}}, rows)
	tm.noteProgramInterrupt("s")
	return tm, ex
}

// Kills an interrupt settle that waits out the scan past the program's own
// boundary (record says settled, scan says busy), or that settles on a
// record the interrupt did not move: under on the idle wait after an
// interrupt returns at the first root boundary past the pre-interrupt
// baseline; under off and shadow the scan alone decides and takes no
// decision read.
func TestInterruptSettleEndsAtTheFirstBoundaryPastTheBaseline(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
	stopped := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnEnds, s.QuietSince = 5, 1, time.Now() })
	rows := func(n int) string {
		if n < 2 {
			return holdPaneRow(working, "", 0)
		}
		return holdPaneRow(stopped, "", 0)
	}
	on, onEx := settleTmux(t, runtime.ProgramStatusOn, rows)
	start := time.Now()
	if err := on.WaitForIdle(context.Background(), "s", 5*time.Second); err != nil {
		t.Fatalf("on: WaitForIdle = %v; want settled on the record", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("on: settled after %s; want it at the record's boundary", took)
	}
	if onEx.holdReads() < 3 {
		t.Fatalf("on: %d record reads; want the baseline and a read each poll", onEx.holdReads())
	}
	// The note is the interrupt's alone: a later idle wait is the scan's.
	if err := on.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("on, a second wait: %v; want the scan's timeout", err)
	}

	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow} {
		tm, ex := settleTmux(t, mode, rows)
		if err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("%s: WaitForIdle = %v; want the scan's timeout", mode, err)
		}
		if ex.holdReads() != 0 {
			t.Fatalf("%s: %d decision reads; want none", mode, ex.holdReads())
		}
	}

	t.Run("record not moved since the interrupt", func(t *testing.T) {
		tm, _ := settleTmux(t, runtime.ProgramStatusOn, func(int) string { return holdPaneRow(working, "", 0) })
		if err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("WaitForIdle = %v; want no settle on an unmoved working record", err)
		}
	})
	t.Run("another nonce", func(t *testing.T) {
		other := psRecord(t, "0th3r.%1", runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 9 })
		tm, _ := settleTmux(t, runtime.ProgramStatusOn, func(n int) string {
			if n == 0 {
				return holdPaneRow(working, "", 0)
			}
			return holdPaneRow(other, "", 0)
		})
		if err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("WaitForIdle = %v; want no settle on another incarnation's record", err)
		}
	})
}

// Kills a settle that sends more keys into a dialog the interrupted program
// raised: under on a live blocked record ends the idle wait at once with an
// error wrapping runtime.ErrPendingInteraction; past its hold bound it
// holds nothing and the scan decides.
func TestInterruptSettleBlockedIsNotSettled(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 5 })
	tm, _ := settleTmux(t, runtime.ProgramStatusOn, func(n int) string {
		if n == 0 {
			return holdPaneRow(working, "", 0)
		}
		return holdPaneRow(blocked, "", 0)
	})
	err := tm.WaitForIdle(context.Background(), "s", 5*time.Second)
	if !errors.Is(err, runtime.ErrPendingInteraction) || errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("WaitForIdle = %v; want a pending-interaction error at once", err)
	}

	stale := psRecord(t, holdNonce, runtime.ProgramBlocked, func(s *runtime.ProgramStatus) {
		s.Seq, s.Since = 5, time.Now().Add(-runtime.ProgramStatusHoldLimit-time.Minute)
	})
	tm, _ = settleTmux(t, runtime.ProgramStatusOn, func(n int) string {
		if n == 0 {
			return holdPaneRow(working, "", 0)
		}
		return holdPaneRow(stale, "", 0)
	})
	if err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("stale blocked: WaitForIdle = %v; want the scan's timeout", err)
	}
}

// Kills a settle judged on the state cache's snapshot: the decision read is
// one fresh list-panes of the session (the hold read's format), never a
// cached reading, and it is taken only under on.
func TestDecisionReadIsFreshAndOnOnly(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince)
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow} {
		tm, ex := holdTmux(mode, &fakeExecutor{}, func(int) string { return holdPaneRow(working, "", 0) })
		if _, ok := tm.programDecide("s"); ok || ex.holdReads() != 0 {
			t.Fatalf("%s: decided with %d reads; want nothing read", mode, ex.holdReads())
		}
	}
	activity := time.Now().Add(-time.Second).Unix()
	tm, ex := holdTmux(runtime.ProgramStatusOn, &fakeExecutor{}, func(int) string { return holdPaneRow(working, "", activity) })
	before := time.Now()
	d, ok := tm.programDecide("s")
	if !ok || !d.reading.Status.Running() || !d.reading.Activity.Equal(time.Unix(activity, 0)) || d.at.Before(before) {
		t.Fatalf("decision %+v ok=%v; want a fresh working reading with the pane's activity, judged after the read", d, ok)
	}
	if ex.holdReads() != 1 || tmuxSubcommand(ex.reads[0]) != "list-panes" {
		t.Fatalf("reads %q; want one list-panes", ex.reads)
	}
}

// Kills a ladder that ignores the record's confirmation of a send the scan
// saw delivered but never busy (record says confirmed, scan says
// unobserved): no busy look follows such a send, so only the post-send
// read can confirm it, and the start records no unconfirmed nudge.
func TestStartupLadderConfirmsADeliveredUnobservedSend(t *testing.T) {
	idle := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince)
	started := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 2, 2 })
	on := runStartupLadder(t, runtime.ProgramStatusOn, []error{ErrNudgeSubmitDeliveredUnobserved}, startupRows(idle, started))
	if on.err != nil || on.count("sendKeys") != 1 || on.count("recordUnconfirmedNudge") != 0 {
		t.Fatalf("on: err %v, calls %v; want the send confirmed by the record, nothing recorded", on.err, on.ops.callMethods())
	}
	off := runStartupLadder(t, runtime.ProgramStatusOff, []error{ErrNudgeSubmitDeliveredUnobserved}, startupRows(idle, started))
	if off.err != nil || off.count("recordUnconfirmedNudge") != 1 {
		t.Fatalf("off: err %v, calls %v; want the scan's warning recorded", off.err, off.ops.callMethods())
	}
}

// Kills a ladder that hands back, or re-types, a nudge it already typed when
// the program reports a dialog after it (design §10.2 row N3: blocked after
// the first Enter is delivered but unobserved), and a busy look that reads a
// blocked record as the nudge landing: the ladder types once, ends delivered
// but unobserved (recorded), and hands nothing back.
func TestStartupLadderBlockedAfterTypingIsDeliveredUnobserved(t *testing.T) {
	idle := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince)
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })
	rows := func(n int) string {
		if n < 2 {
			return holdPaneRow(idle, "", 0)
		}
		return holdPaneRow(blocked, "", 0)
	}
	got := runStartupLadder(t, runtime.ProgramStatusOn, []error{ErrNudgeSubmitUnconfirmed}, rows)
	if got.err != nil {
		t.Fatalf("start failed %v; a dialog never fails a start", got.err)
	}
	if got.count("sendKeys") != 1 || got.count("recordUnconfirmedNudge") != 1 || got.handed {
		t.Fatalf("calls %v, handed %v (%q); want one send, the unobserved delivery recorded, nothing handed back", got.ops.callMethods(), got.handed, got.handback)
	}
}

// Kills a settle that reports a dialog the interrupt is canceling: the
// baseline's own blocked record, unchanged since the interrupt (the relay
// has not republished yet), decides nothing and leaves the scan to wait; the
// record the interrupt moves to a boundary then settles it. A blocked record
// that never moves is the scan's to time out, never a pending interaction.
func TestInterruptSettleUnmovedBlockedBaselineIsTheScans(t *testing.T) {
	dialog := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 5 })
	cleared := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnEnds, s.QuietSince = 6, 1, time.Now() })
	tm, _ := settleTmux(t, runtime.ProgramStatusOn, func(n int) string {
		if n < 3 {
			return holdPaneRow(dialog, "", 0)
		}
		return holdPaneRow(cleared, "", 0)
	})
	if err := tm.WaitForIdle(context.Background(), "s", 5*time.Second); err != nil {
		t.Fatalf("WaitForIdle = %v; want settled once the interrupt cleared the dialog", err)
	}
	tm, _ = settleTmux(t, runtime.ProgramStatusOn, func(int) string { return holdPaneRow(dialog, "", 0) })
	err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("WaitForIdle = %v; want the scan's timeout, no pending interaction from the baseline's own record", err)
	}
}

// Kills an interrupt note that outlives its interrupt's own wait: a wait
// that comes past programInterruptWindow is the scan's, and takes no
// decision read.
func TestInterruptSettleNoteExpires(t *testing.T) {
	stopped := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 5 })
	tm, ex := holdTmux(runtime.ProgramStatusOn, &paneScriptExecutor{captures: []scriptedCapture{{out: busyPane}}}, func(int) string { return holdPaneRow(stopped, "", 0) })
	baseline := runtime.ProgramBaseline{Nonce: holdNonce, Seq: 4, State: runtime.ProgramWorking}
	tm.keepInterrupt("s", shadowInterrupt{baseline: baseline, at: time.Now().Add(-programInterruptWindow - time.Second)})
	if err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("WaitForIdle = %v; want the scan's timeout past the note's window", err)
	}
	if ex.holdReads() != 0 {
		t.Fatalf("%d decision reads after the note expired; want none", ex.holdReads())
	}
}

// Kills a Start or Relaunch that leaves a handback its caller may take after
// the start failed: each clears the session's handback before anything can
// return, and a failed one hands nothing back.
func TestFailedStartHandsNothingBack(t *testing.T) {
	ex := &fakeExecutor{err: errors.New("no server")}
	cfg := DefaultConfig()
	tm := &Tmux{cfg: cfg, exec: ex}
	p := &Provider{tm: tm, cfg: cfg, cache: NewStateCache(nil, time.Hour), workDirs: map[string]string{}}
	tm.handbacks.set("s", "an earlier start's nudge")
	if err := p.Start(context.Background(), "s", runtime.Config{Command: "agent", Nudge: "start working"}); err == nil {
		t.Fatal("Start succeeded over a failing server")
	}
	if nudge, ok := p.TakeStartupNudgeHandback("s"); ok {
		t.Fatalf("failed Start handed back %q", nudge)
	}
	tm.handbacks.set("s", "an earlier start's nudge")
	if err := p.Relaunch(context.Background(), "s", runtime.Config{Command: "agent", Nudge: "start working"}); err == nil {
		t.Fatal("Relaunch succeeded over a failing server")
	}
	if nudge, ok := p.TakeStartupNudgeHandback("s"); ok {
		t.Fatalf("failed Relaunch handed back %q", nudge)
	}
}
