package tmux

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// nudgeReads is one shadowing Provider.Nudge's program reads: how many it
// took before its paste, and how many by the time it returned.
type nudgeReads struct {
	beforePaste, total int
}

// isPaste reports whether c, a command in shadowExecutor.order, is a
// nudge's literal paste.
func isPaste(c string) bool {
	return strings.Contains(c, "send-keys") && strings.Contains(c, " -l ")
}

// readsAround counts the program reads in order (shadowExecutor.order)
// before the first paste, and in all.
func readsAround(order []string) nudgeReads {
	paste := slices.IndexFunc(order, isPaste)
	if paste < 0 {
		paste = len(order)
	}
	var r nudgeReads
	for i, c := range order {
		if c != "program-read" {
			continue
		}
		r.total++
		if i < paste {
			r.beforePaste++
		}
	}
	return r
}

// runShadowNudge runs one Provider.Nudge of a Claude session at an idle
// prompt under mode, its idle wait on, with an idle wait decided outside the
// leaf deferred to it first when outerWait. It returns the program reads the
// nudge took on its path and the commands the pane was sent.
func runShadowNudge(t *testing.T, mode runtime.ProgramStatusMode, sink runtime.ProgramStatusComparisonSink, outerWait bool) (nudgeReads, [][]string, error) {
	t.Helper()
	const session = "shadow-nudge-reads"
	var reads nudgeReads
	var calls [][]string
	var err error
	synctest.Test(t, func(t *testing.T) {
		idle := psRecord(t, readNonce, runtime.ProgramIdle, liveSince)
		started := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 2, 1 })
		inner := &agentPaneExecutor{
			session:  session,
			provider: "claude",
			attached: "0",
			pane:     []string{"● Done.", "────", "❯ ", "────", "  ⏵⏵ bypass permissions on"},
		}
		// A read answers under the executor's lock, so it sees every
		// command sent before it: the record starts a turn once the
		// message is pasted.
		var ex *shadowExecutor
		record := func(int) string {
			if slices.ContainsFunc(ex.order, isPaste) {
				return started
			}
			return idle
		}
		var tm *Tmux
		tm, ex = shadowTmux(mode, inner, record, sink)
		tm.cfg.NudgeIdleTimeout = 2 * time.Second
		p := &Provider{tm: tm}
		if outerWait && !p.ProgramStatusShadowIdleWaitAtNudge(session, time.Now().Add(-time.Second), time.Now(), runtime.ProgramVerdictIdle) {
			if mode == runtime.ProgramStatusShadow {
				t.Fatal("a shadowing leaf deferred no idle wait")
			}
		}
		err = p.Nudge(session, runtime.TextContent("reminder: please respond to the review"))
		ex.mu.Lock()
		reads = readsAround(slices.Clone(ex.order))
		ex.mu.Unlock()
		synctest.Wait()
		sessionNudgeLocks.Delete(session)
		calls = inner.calls
	})
	return reads, calls, err
}

// Kills a shadowing nudge that reads the program's record more than once
// before it types (ga-f4ovjm.77: the second shadow measured about 160 ms of
// synchronous list-panes per nudge): the nudge's idle wait, an idle wait
// decided outside the leaf whose delivery it carries, its typing gate and
// the baseline its submit is confirmed against are one decision, judged on
// one fresh read taken just before the paste. The submit's confirmation is
// the nudge's one other decision, with one read of its own after the
// submit. The nudge sends the pane exactly what it sends with the switch off,
// which reads nothing.
func TestShadowNudgeTakesOneDecisionRead(t *testing.T) {
	for _, outerWait := range []bool{false, true} {
		name := "own idle wait"
		if outerWait {
			name = "own and a deferred outer idle wait"
		}
		t.Run(name, func(t *testing.T) {
			offReads, offCalls, offErr := runShadowNudge(t, runtime.ProgramStatusOff, nil, outerWait)
			col := newShadowCollector()
			reads, calls, err := runShadowNudge(t, runtime.ProgramStatusShadow, col.sink, outerWait)
			if (offErr == nil) != (err == nil) {
				t.Fatalf("shadow Nudge = %v, off = %v: the outcome changed", err, offErr)
			}
			if !slices.EqualFunc(offCalls, calls, slices.Equal) {
				t.Fatalf("shadow sent the pane other commands than off:\nshadow %q\noff    %q", calls, offCalls)
			}
			if offReads.total != 0 {
				t.Fatalf("the switch off took %d program reads, want none", offReads.total)
			}
			if reads.beforePaste != 1 || reads.total != 2 {
				t.Fatalf("shadow nudge took %d program reads before its paste and %d in all, want 1 and 2", reads.beforePaste, reads.total)
			}
			waits := 1
			if outerWait {
				waits = 2
			}
			got := col.wait(t, waits+2)
			var typing runtime.ProgramStatusComparison
			byPoint := map[runtime.ProgramStatusPoint]int{}
			for _, c := range got {
				byPoint[c.Point]++
				if c.Point == runtime.ProgramPointTypingGate {
					typing = c
				}
			}
			if byPoint[runtime.ProgramPointWaitForIdle] != waits || byPoint[runtime.ProgramPointTypingGate] != 1 || byPoint[runtime.ProgramPointSubmitConfirm] != 1 {
				t.Fatalf("comparisons by point %v, want %d wait_for_idle, one typing_gate, one submit_confirm", byPoint, waits)
			}
			for _, c := range got {
				if c.Point != runtime.ProgramPointWaitForIdle {
					continue
				}
				if c.Status != typing.Status || !c.JudgedAt.Equal(typing.JudgedAt) || c.Program != runtime.ProgramVerdictIdle {
					t.Fatalf("idle wait %+v judged on another reading than the nudge's decision reading %+v", c, typing)
				}
			}
		})
	}
}

// Kills an idle wait deferred to a nudge that never takes its decision
// reading, and one judged on a read taken after the nudge typed: an outer
// wait whose delivery never reached the leaf is judged when its caller
// settles it, on one read; one the nudge judged is not judged again.
func TestShadowNudgeSettlesAnUnreadDeferredWait(t *testing.T) {
	col := newShadowCollector()
	idle := psRecord(t, readNonce, runtime.ProgramIdle, liveSince)
	tm, ex := shadowTmux(runtime.ProgramStatusShadow, &paneScriptExecutor{captures: []scriptedCapture{{out: "❯ "}}}, func(int) string { return idle }, col.sink)
	p := &Provider{tm: tm}
	if !p.ProgramStatusShadowIdleWaitAtNudge("s", time.Now().Add(-time.Second), time.Now(), runtime.ProgramVerdictIdle) {
		t.Fatal("a shadowing leaf deferred no idle wait")
	}
	if ex.programReads() != 0 {
		t.Fatalf("deferring took %d reads", ex.programReads())
	}
	p.ProgramStatusShadowSettleNudge("s")
	p.ProgramStatusShadowSettleNudge("s")
	if ex.programReads() != 1 {
		t.Fatalf("settling took %d reads, want one", ex.programReads())
	}
	if got := col.wait(t, 1); got[0].Point != runtime.ProgramPointWaitForIdle || got[0].Program != runtime.ProgramVerdictIdle {
		t.Fatalf("comparison %+v, want wait_for_idle idle/idle", got[0])
	}

	off, offEx := shadowTmux(runtime.ProgramStatusOff, &paneScriptExecutor{}, func(int) string { return idle }, nil)
	offP := &Provider{tm: off}
	if offP.ProgramStatusShadowIdleWaitAtNudge("s", time.Now(), time.Now(), runtime.ProgramVerdictIdle) {
		t.Fatal("a leaf that is not shadowing deferred an idle wait")
	}
	offP.ProgramStatusShadowSettleNudge("s")
	if offEx.programReads() != 0 {
		t.Fatalf("the switch off took %d reads", offEx.programReads())
	}
}

// Kills a background comparison (observation) that runs twice at once for
// one session and point, or that runs on the caller's path.
func TestShadowGoOneAtATimePerSessionPoint(t *testing.T) {
	tm, _ := shadowTmux(runtime.ProgramStatusShadow, &paneScriptExecutor{}, nil, newShadowCollector().sink)
	p := &Provider{tm: tm}
	release := make(chan struct{})
	ran := make(chan string, 4)
	p.ProgramStatusShadowGo("s", runtime.ProgramPointObservation, func() { ran <- "first"; <-release })
	if got := <-ran; got != "first" {
		t.Fatalf("ran %q", got)
	}
	p.ProgramStatusShadowGo("s", runtime.ProgramPointObservation, func() { ran <- "coalesced" })
	p.ProgramStatusShadowGo("t", runtime.ProgramPointObservation, func() { ran <- "other session" })
	if got := <-ran; got != "other session" {
		t.Fatalf("ran %q, want the other session's", got)
	}
	close(release)
	deadline := time.After(10 * time.Second)
	for {
		p.ProgramStatusShadowGo("s", runtime.ProgramPointObservation, func() { ran <- "after" })
		select {
		case got := <-ran:
			if got != "after" {
				t.Fatalf("ran %q while the first was running", got)
			}
			return
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("the session's point was never released")
		}
	}
}
