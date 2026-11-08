package tmux

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// decidedStartOps are start ops whose startup ladder decides with tm's
// program record (Tmux.programStartupLadder) and hand a held-back nudge to
// tm, as tmuxStartOps do.
type decidedStartOps struct {
	*fakeStartOps
	tm *Tmux
}

func (o decidedStartOps) programStartupLadder(name string, arm runtime.ProgramStatusArm, send func() error, busy func() (bool, error)) (func() error, func() (bool, error)) {
	return o.tm.programStartupLadder(name, arm, send, busy)
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

// decidingStartupArm is the arm of a start whose provider opts the startup
// ladder into deciding with the record (program_status decide_startup).
var decidingStartupArm = runtime.ProgramStatusArm{Capture: true, DecideStartup: true}

func runStartupLadder(t *testing.T, mode runtime.ProgramStatusMode, sends []error, rows func(n int) string) startupRun {
	t.Helper()
	return runStartupLadderArmed(t, mode, decidingStartupArm, sends, rows)
}

// runStartupLadderArmed is runStartupLadder for a start carrying arm.
func runStartupLadderArmed(t *testing.T, mode runtime.ProgramStatusMode, arm runtime.ProgramStatusArm, sends []error, rows func(n int) string) startupRun {
	t.Helper()
	fastStartupLadder(t)
	tm, ex := holdTmux(mode, &fakeExecutor{}, rows)
	ops := &fakeStartOps{hasSessionResult: true, sendKeysErrs: slices.Clone(sends)}
	cfg := runtime.Config{Command: "claude", Nudge: "start working", ProgramStatus: arm}
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
			return settlingPaneRow(base)
		}
		return settlingPaneRow(later)
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

// Kills a startup ladder that decides with the record for a provider that
// did not opt into it (program_status decide_startup unset): under on, a
// start whose arm declares capture (and the hold) but not the startup
// decision runs the scan's ladder exactly as under off. It reads no record,
// re-sends a nudge the scan could not confirm though the record shows the
// turn start, types into a dialog the record reports, and hands nothing
// back.
func TestStartupLadderUndeclaredDecidesAsOff(t *testing.T) {
	idle := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince)
	started := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 2, 2 })
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })
	undecided := runtime.ProgramStatusArm{Capture: true, HoldMidTurn: true, DecideSettle: true}
	for name, c := range map[string]struct {
		sends []error
		rows  func(int) string
	}{
		"turn start":     {[]error{ErrNudgeSubmitUnconfirmed, nil}, startupRows(idle, started)},
		"blocked record": {[]error{ErrNudgeSubmitUnconfirmed, nil}, startupRows(idle, blocked)},
	} {
		t.Run(name, func(t *testing.T) {
			off := runStartupLadderArmed(t, runtime.ProgramStatusOff, undecided, c.sends, c.rows)
			on := runStartupLadderArmed(t, runtime.ProgramStatusOn, undecided, c.sends, c.rows)
			if on.err != nil || off.err != nil {
				t.Fatalf("start failed: on %v, off %v", on.err, off.err)
			}
			if !slices.Equal(on.ops.callMethods(), off.ops.callMethods()) {
				t.Fatalf("on calls %v, off calls %v; want the scan's ladder unchanged", on.ops.callMethods(), off.ops.callMethods())
			}
			if on.count("sendKeys") != 2 || on.reads != 0 || on.handed {
				t.Fatalf("on: %d sends, %d reads, handed back %v; want the scan's two sends, no read, nothing handed back", on.count("sendKeys"), on.reads, on.handed)
			}
		})
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
				return settlingPaneRow(idle)
			case n <= 2:
				return settlingPaneRow(blocked)
			}
			return settlingPaneRow(answered)
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
			return settlingPaneRow(working)
		}
		return settlingPaneRow(stopped)
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
		tm, _ := settleTmux(t, runtime.ProgramStatusOn, func(int) string { return settlingPaneRow(working) })
		if err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("WaitForIdle = %v; want no settle on an unmoved working record", err)
		}
	})
	t.Run("another nonce", func(t *testing.T) {
		other := psRecord(t, "0th3r.%1", runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 9 })
		tm, _ := settleTmux(t, runtime.ProgramStatusOn, func(n int) string {
			if n == 0 {
				return settlingPaneRow(working)
			}
			return settlingPaneRow(other)
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
			return settlingPaneRow(working)
		}
		return settlingPaneRow(blocked)
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
			return settlingPaneRow(working)
		}
		return settlingPaneRow(stale)
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
		tm, ex := holdTmux(mode, &fakeExecutor{}, func(int) string { return settlingPaneRow(working) })
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
			return settlingPaneRow(idle)
		}
		return settlingPaneRow(blocked)
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
			return settlingPaneRow(dialog)
		}
		return settlingPaneRow(cleared)
	})
	if err := tm.WaitForIdle(context.Background(), "s", 5*time.Second); err != nil {
		t.Fatalf("WaitForIdle = %v; want settled once the interrupt cleared the dialog", err)
	}
	tm, _ = settleTmux(t, runtime.ProgramStatusOn, func(int) string { return settlingPaneRow(dialog) })
	err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, runtime.ErrPendingInteraction) {
		t.Fatalf("WaitForIdle = %v; want the scan's timeout, no pending interaction from the baseline's own record", err)
	}
}

// Kills an interrupt settle that decides with the record on a pane whose
// provider did not opt into it (no settle declaration bound to the pane, or
// one inherited from an outer scope naming another pane): under on, a
// dialog the record reports after the interrupt ends nothing, and a
// boundary the record reports past the baseline settles nothing; the scan
// alone decides, as under off. The hold declaration does not stand in for
// the settle's.
func TestInterruptSettleUndeclaredPaneDecidesAsOff(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 5 })
	stopped := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnEnds, s.QuietSince = 5, 1, time.Now() })
	for _, settle := range []string{"", "%9"} {
		for name, later := range map[string]string{"dialog": blocked, "boundary": stopped} {
			t.Run(name+" settle="+settle, func(t *testing.T) {
				tm, ex := holdTmux(runtime.ProgramStatusOn, &paneScriptExecutor{captures: []scriptedCapture{{out: busyPane}}}, func(n int) string {
					if n == 0 {
						return declaredPaneRow(working, "%1", settle, 0)
					}
					return declaredPaneRow(later, "%1", settle, 0)
				})
				tm.noteProgramInterrupt("s")
				if err := tm.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
					t.Fatalf("WaitForIdle = %v; want the scan's timeout", err)
				}
				if n := ex.holdReads(); n != 1 {
					t.Fatalf("%d record reads; want only the note's own, which finds no declaration", n)
				}
			})
		}
	}
}

// Kills an interrupt settle that keeps deciding with the record after heal
// withdrew the pane's settle declaration: heal unsets it under the same
// nonce (declarePane), so neither the note's nonce nor its sequence tells.
// A reading whose pane no longer declares the settle decides nothing, and
// the scan alone decides, as under off, whether the declaration went before
// the wait began or during it: a boundary past the baseline settles nothing
// and a dialog raised since ends nothing.
func TestInterruptSettleWithdrawnDeclarationDecidesAsOff(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 5 })
	stopped := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnEnds, s.QuietSince = 5, 1, time.Now() })
	for _, c := range []struct {
		when string
		// withdrawn is the first read whose pane no longer declares the
		// settle; read 0 is the note's own.
		withdrawn int
		timeout   time.Duration
	}{
		{"before the wait", 1, 300 * time.Millisecond},
		{"during the wait", 2, 700 * time.Millisecond},
	} {
		for name, later := range map[string]string{"dialog": blocked, "boundary": stopped} {
			t.Run(c.when+" "+name, func(t *testing.T) {
				tm, ex := settleTmux(t, runtime.ProgramStatusOn, func(n int) string {
					if n < c.withdrawn {
						return settlingPaneRow(working)
					}
					return declaredPaneRow(later, "", "", 0)
				})
				err := tm.WaitForIdle(context.Background(), "s", c.timeout)
				if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, runtime.ErrPendingInteraction) {
					t.Fatalf("WaitForIdle = %v; want the scan's timeout", err)
				}
				if n := ex.holdReads(); n <= c.withdrawn {
					t.Fatalf("%d record reads; want the wait to read the withdrawn declaration", n)
				}
			})
		}
	}
}

// Kills an interrupt note that outlives its interrupt's own wait: a wait
// that comes past programInterruptWindow is the scan's, and takes no
// decision read.
func TestInterruptSettleNoteExpires(t *testing.T) {
	stopped := psRecord(t, holdNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 5 })
	tm, ex := holdTmux(runtime.ProgramStatusOn, &paneScriptExecutor{captures: []scriptedCapture{{out: busyPane}}}, func(int) string { return settlingPaneRow(stopped) })
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

// softInterruptTmux is a Tmux under mode over a Codex pane whose scan reads
// busy throughout, its fresh record reads answered by rows with the keys
// that reached the pane so far.
func softInterruptTmux(t *testing.T, mode runtime.ProgramStatusMode, rows func(pressed []string) string) (*Tmux, *fakeCodexPane, *holdExecutor) {
	t.Helper()
	pane := newFakeCodexPane(t)
	pane.busyCaptures = 1 << 30
	tm := newCodexStopTmux(pane)
	tm.cfg.ProgramStatus = mode
	tm.capture.probe = passGate
	ex := &holdExecutor{inner: pane, row: func(int) string { return rows(pane.typedOrPressed()) }}
	tm.exec = ex
	return tm, pane, ex
}

// dialogAfterTheEscape answers a working root (Seq 4) until an Escape reached
// the pane, and a blocked one (Seq 5) once one has: a dialog the program
// raised since a soft interrupt's Escape.
func dialogAfterTheEscape(t *testing.T) func(pressed []string) string {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
	blocked := psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 5 })
	return func(pressed []string) string {
		if slices.Contains(pressed, "key:Escape") {
			return settlingPaneRow(blocked)
		}
		return settlingPaneRow(working)
	}
}

// Kills a soft interrupt whose idle wait the record cannot settle, and an
// Escape taken as an interrupt it is not: under on, NoteInterrupt before a
// soft interrupt's Escape takes the pre-interrupt baseline (one fresh read),
// so the idle wait after the Escape ends at once on a dialog the program
// raised since; an Escape sent with no note (a dialog dismissed, an input
// cleared) leaves that wait the scan's, with no decision read; and under off
// and shadow the note reads nothing.
func TestNoteInterruptArmsTheSettleOfASoftInterrupt(t *testing.T) {
	tm, _, ex := softInterruptTmux(t, runtime.ProgramStatusOn, dialogAfterTheEscape(t))
	p := &Provider{tm: tm}
	p.NoteInterrupt("s")
	if n := ex.holdReads(); n != 1 {
		t.Fatalf("NoteInterrupt took %d decision reads; want the baseline's one", n)
	}
	if err := p.SendKeys("s", "Escape"); err != nil {
		t.Fatalf("SendKeys(Escape): %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 5*time.Second); !errors.Is(err, runtime.ErrPendingInteraction) || errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("noted: WaitForIdle = %v; want a pending-interaction error at once", err)
	}

	tm, _, ex = softInterruptTmux(t, runtime.ProgramStatusOn, dialogAfterTheEscape(t))
	p = &Provider{tm: tm}
	if err := p.SendKeys("s", "Escape"); err != nil {
		t.Fatalf("SendKeys(Escape): %v", err)
	}
	if err := p.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("unnoted Escape: WaitForIdle = %v; want the scan's timeout", err)
	}
	if n := ex.holdReads(); n != 0 {
		t.Fatalf("unnoted Escape: %d decision reads; want none", n)
	}

	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow} {
		tm, _, ex := softInterruptTmux(t, mode, dialogAfterTheEscape(t))
		p := &Provider{tm: tm}
		p.NoteInterrupt("s")
		if err := p.SendKeys("s", "Escape"); err != nil {
			t.Fatalf("%s: SendKeys(Escape): %v", mode, err)
		}
		if err := p.WaitForIdle(context.Background(), "s", 300*time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
			t.Fatalf("%s: WaitForIdle = %v; want the scan's timeout", mode, err)
		}
		if n := ex.holdReads(); n != 0 {
			t.Fatalf("%s: %d decision reads; want none", mode, n)
		}
	}
}

// softInterruptSessionProvider is a session runtime whose keys, interrupts,
// interrupt notes and idle waits reach a tmux Provider (softInterruptTmux),
// while the session lifecycle, its other reads and message delivery stay on
// a runtime.Fake.
type softInterruptSessionProvider struct {
	*runtime.Fake
	tp *Provider
}

func (p *softInterruptSessionProvider) SendKeys(name string, keys ...string) error {
	return p.tp.SendKeys(name, keys...)
}

func (p *softInterruptSessionProvider) Interrupt(name string) error { return p.tp.Interrupt(name) }

func (p *softInterruptSessionProvider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	return p.tp.WaitForIdle(ctx, name, timeout)
}

func (p *softInterruptSessionProvider) NoteInterrupt(name string) { p.tp.NoteInterrupt(name) }

// Kills a soft interrupt whose Ctrl-C fallback lands in a dialog the program
// raised after its Escape (record says blocked past the pre-Escape baseline,
// scan says busy), end to end from the session: under on, interrupt_now on a
// Codex session notes the interrupt at the leaf before its Escape, so the
// idle wait after the Escape ends on the dialog, no Ctrl-C reaches the pane,
// and the replacing message is queued behind the dialog. A note taken after
// the Escape would read the dialog as its own baseline and settle nothing.
func TestInterruptNowQueuesBehindADialogRaisedAfterTheSoftEscape(t *testing.T) {
	tm, pane, _ := softInterruptTmux(t, runtime.ProgramStatusOn, dialogAfterTheEscape(t))
	sp := &softInterruptSessionProvider{Fake: runtime.NewFake(), tp: &Provider{tm: tm}}
	cityPath := t.TempDir()
	mgr := session.NewManagerWithOptions(beadstest.Sessions(beads.NewMemStore()), sp, session.WithCityPath(cityPath))
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "helper", Command: "codex", WorkDir: t.TempDir(), Provider: "codex", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	city, err := session.NewCityDir(cityPath)
	if err != nil {
		t.Fatalf("NewCityDir: %v", err)
	}
	before := len(sp.SnapshotCalls())

	outcome, err := mgr.Submit(context.Background(), session.Actor{Kind: session.ActorOperator, City: city}, info.ID, "do this instead", session.BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, session.SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v; want the message queued behind the dialog", err)
	}
	if !outcome.Queued {
		t.Fatalf("Submit(interrupt_now) = %+v; want the message queued behind the dialog", outcome)
	}
	if got, want := pane.typedOrPressed(), []string{"key:Escape"}; !slices.Equal(got, want) {
		t.Fatalf("keys reaching the Codex pane = %q, want %q: no Ctrl-C fallback into the dialog", got, want)
	}
	for _, c := range sp.SnapshotCalls()[before:] {
		switch c.Method {
		case "Stop", "Start", "Nudge", "NudgeNow":
			t.Fatalf("runtime call %s after the settle reported a dialog; want none", c.Method)
		}
	}
}
