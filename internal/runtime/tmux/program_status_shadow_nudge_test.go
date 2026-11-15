package tmux

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A shadowing nudge records each of its decisions on a reading taken at that
// decision: an idle wait that let the delivery go (the leaf's own, inside
// Nudge, and the op lane's, decided outside the leaf) on one taken as the
// wait decided; the typing gate and the submit's baseline on one taken just
// before the paste; the submit's confirmation on one of its own. A wait
// judged at the paste instead judges whatever came between: the nudge lock
// another nudge held, gc's dismissals and composer clear, another delivery's
// submit.

// shadowIdleClaudePane is a Claude pane at an idle prompt.
var shadowIdleClaudePane = []string{"● Done.", "────", "❯ ", "────", "  ⏵⏵ bypass permissions on"}

// isPaste reports whether c, a command in shadowExecutor.order, is a
// nudge's literal paste.
func isPaste(c string) bool {
	return strings.Contains(c, "send-keys") && strings.Contains(c, " -l ")
}

// isSubmit reports whether c, a command in shadowExecutor.order, is a
// nudge's submit Enter.
func isSubmit(c string) bool {
	return strings.Contains(c, "send-keys") && strings.HasSuffix(c, " Enter")
}

// readsBefore counts the program reads in order (shadowExecutor.order)
// before the first command at reports true for, and in all.
func readsBefore(order []string, at func(string) bool) (before, total int) {
	end := slices.IndexFunc(order, at)
	if end < 0 {
		end = len(order)
	}
	for i, c := range order {
		if c != "program-read" {
			continue
		}
		total++
		if i < end {
			before++
		}
	}
	return before, total
}

// lockedNudge is what one runLockedShadowNudge did: the program reads it
// took before the nudge's paste and in all, the commands the pane was sent,
// and the nudge's error.
type lockedNudge struct {
	beforePaste, reads int
	calls              [][]string
	err                error
}

// runLockedShadowNudge runs one Provider.Nudge of a Claude session at an
// idle prompt under mode, its comparisons going to sink, in a process that
// follows no comparison up: every program read is a decision's. ownWait
// turns on the nudge's own idle wait, which the production leaf leaves off
// (NudgeIdleTimeout 0); outerWait has an idle wait decided outside the leaf
// (the op lane's) recorded first. Another nudge holds the session's nudge
// lock from before either wait decides, and the turn the record reports
// running when they decide ends before it lets go: the record reads working
// (Seq 2) until then, idle at a boundary (Seq 3) after, and a new turn
// (Seq 4) once the message is pasted.
func runLockedShadowNudge(t *testing.T, mode runtime.ProgramStatusMode, sink runtime.ProgramStatusComparisonSink, ownWait, outerWait bool) lockedNudge {
	t.Helper()
	const session = "shadow-nudge-locked"
	const lockHeld = 2 * time.Second
	var got lockedNudge
	synctest.Test(t, func(t *testing.T) {
		t0 := time.Now()
		working := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })
		ended := psRecord(t, readNonce, runtime.ProgramIdle, func(s *runtime.ProgramStatus) {
			s.Seq, s.TurnStarts, s.TurnEnds = 3, 1, 1
			s.Since, s.QuietSince = t0.Add(lockHeld), t0.Add(lockHeld)
		})
		started := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts, s.TurnEnds = 4, 2, 1 })
		var turnEnded atomic.Bool
		// A read answers under the executor's lock, so it sees every
		// command sent before it.
		var ex *shadowExecutor
		record := func(int) string {
			switch {
			case slices.ContainsFunc(ex.order, isPaste):
				return started
			case turnEnded.Load():
				return ended
			default:
				return working
			}
		}
		inner := &agentPaneExecutor{session: session, provider: "claude", attached: "0", pane: shadowIdleClaudePane}
		var tm *Tmux
		tm, ex = shadowTmux(mode, inner, record, sink)
		tm.cfg.ComparisonFollowUps = nil
		tm.cfg.NudgeIdleTimeout = 0
		if ownWait {
			tm.cfg.NudgeIdleTimeout = 2 * time.Second
		}
		p := &Provider{tm: tm}
		go func() {
			if !acquireNudgeLock(session, time.Minute) {
				t.Error("the other nudge did not take the session's nudge lock")
				return
			}
			time.Sleep(lockHeld)
			turnEnded.Store(true)
			releaseNudgeLock(session)
		}()
		synctest.Wait()
		if outerWait {
			p.ProgramStatusShadowIdleWait(session, t0.Add(-time.Second), t0, runtime.ProgramVerdictIdle)
		}
		got.err = p.Nudge(session, runtime.TextContent("reminder: please respond to the review"))
		ex.mu.Lock()
		got.beforePaste, got.reads = readsBefore(ex.order, isPaste)
		ex.mu.Unlock()
		synctest.Wait()
		// The bubble created this session's nudge lock; the next run's
		// bubble must make its own.
		sessionNudgeLocks.Delete(session)
		got.calls = inner.calls
	})
	return got
}

// Kills a shadowing nudge that judges an idle wait that let it go on a
// reading taken after the wait decided (review of ga-f4ovjm.77: waits were
// judged on the nudge's read just before its paste). While another nudge
// holds the session's nudge lock, the turn the record reported running when
// the wait decided ends: judged at the paste, the wait would record the
// record's idle beside the scrape's, hiding the lag the point measures, and
// its time is taken from the decision. Each wait is judged on the record as
// it stood when it decided; the typing gate and the submit's baseline share
// one read taken just before the paste, and the submit's confirmation takes
// its own: one read per decision. The nudge sends the pane exactly what it
// sends with the switch off, which reads nothing.
func TestShadowNudgeJudgesEachWaitAtItsDecision(t *testing.T) {
	for _, c := range []struct {
		name               string
		ownWait, outerWait bool
	}{
		{"the op lane's wait, none of its own (production)", false, true},
		{"its own idle wait", true, false},
		{"its own and the op lane's wait", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			off := runLockedShadowNudge(t, runtime.ProgramStatusOff, nil, c.ownWait, c.outerWait)
			col := newShadowCollector()
			sh := runLockedShadowNudge(t, runtime.ProgramStatusShadow, col.sink, c.ownWait, c.outerWait)
			if (off.err == nil) != (sh.err == nil) {
				t.Fatalf("shadow Nudge = %v, off = %v: the outcome changed", sh.err, off.err)
			}
			if !slices.EqualFunc(off.calls, sh.calls, slices.Equal) {
				t.Fatalf("shadow sent the pane other commands than off:\nshadow %q\noff    %q", sh.calls, off.calls)
			}
			if off.reads != 0 {
				t.Fatalf("the switch off took %d program reads, want none", off.reads)
			}
			waits := 0
			for _, on := range []bool{c.ownWait, c.outerWait} {
				if on {
					waits++
				}
			}
			if sh.beforePaste != waits+1 || sh.reads != waits+2 {
				t.Fatalf("shadow took %d program reads before the paste and %d in all, want %d and %d: one per decision", sh.beforePaste, sh.reads, waits+1, waits+2)
			}
			byPoint := map[runtime.ProgramStatusPoint][]runtime.ProgramStatusComparison{}
			for _, cmp := range col.wait(t, waits+2) {
				byPoint[cmp.Point] = append(byPoint[cmp.Point], cmp)
			}
			if len(byPoint[runtime.ProgramPointWaitForIdle]) != waits || len(byPoint[runtime.ProgramPointTypingGate]) != 1 || len(byPoint[runtime.ProgramPointSubmitConfirm]) != 1 {
				t.Fatalf("comparisons by point %v, want %d wait_for_idle, one typing_gate, one submit_confirm", byPoint, waits)
			}
			for _, w := range byPoint[runtime.ProgramPointWaitForIdle] {
				if w.Scrape != runtime.ProgramVerdictIdle || w.Program != runtime.ProgramVerdictBusy || w.Status.Seq != 2 {
					t.Fatalf("idle wait %+v, want idle/busy on the working record it decided beside, not a reading taken after it decided", w)
				}
			}
			if g := byPoint[runtime.ProgramPointTypingGate][0]; g.Status.Seq != 3 {
				t.Fatalf("typing gate %+v, want it judged on the read just before the paste", g)
			}
		})
	}
}

// afterPaste is an executor that runs hook once, after the first paste it
// passes to next returned and outside next's lock, so the hook can start
// work that sends commands of its own.
type afterPaste struct {
	next executor
	once sync.Once
	hook func()
}

func (e *afterPaste) execute(args []string) (string, error) {
	out, err := e.next.execute(args)
	if isPaste(strings.Join(args, " ")) {
		e.once.Do(e.hook)
	}
	return out, err
}

func (e *afterPaste) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// Kills shadowing nudges to one session whose idle waits are judged on each
// other's readings (review of ga-f4ovjm.77: deferred waits were kept per
// session, so one nudge's settle judged another's wait after its own
// submit). A second nudge's own wait decides while the first, holding the
// session's nudge lock, has pasted and not yet submitted; then the first's
// submit starts a turn. Each wait is judged on the record as it stood when
// it decided, before any submit, never on a read either nudge takes after
// one.
func TestShadowOverlappingNudgesJudgeTheirOwnWaits(t *testing.T) {
	const session = "shadow-nudge-overlap"
	col := newShadowCollector()
	var order []string
	synctest.Test(t, func(t *testing.T) {
		idle := psRecord(t, readNonce, runtime.ProgramIdle, liveSince)
		submitted := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 2, 1 })
		var ex *shadowExecutor
		record := func(int) string {
			if slices.ContainsFunc(ex.order, isSubmit) {
				return submitted
			}
			return idle
		}
		inner := &agentPaneExecutor{session: session, provider: "claude", attached: "0", pane: shadowIdleClaudePane}
		var tm *Tmux
		tm, ex = shadowTmux(runtime.ProgramStatusShadow, inner, record, col.sink)
		tm.cfg.ComparisonFollowUps = nil
		tm.cfg.NudgeIdleTimeout = 2 * time.Second
		p := &Provider{tm: tm}
		second := make(chan error, 1)
		// The first nudge's paste starts the second, and the first pauses
		// there, long past the second's wait, which then waits on the
		// session's nudge lock.
		tm.exec = &afterPaste{next: ex, hook: func() {
			go func() { second <- p.Nudge(session, runtime.TextContent("second")) }()
			time.Sleep(5 * time.Second)
		}}
		_ = p.Nudge(session, runtime.TextContent("first"))
		<-second
		synctest.Wait()
		sessionNudgeLocks.Delete(session)
		ex.mu.Lock()
		order = slices.Clone(ex.order)
		ex.mu.Unlock()
	})
	if before, _ := readsBefore(order, isSubmit); before != 3 {
		t.Fatalf("%d program reads before the first submit, want 3 (each nudge's wait, the first's typing gate): the second's wait must decide before it\n%q", before, order)
	}
	got := col.wait(t, 6)
	var waits []runtime.ProgramStatusComparison
	for _, c := range got {
		if c.Point == runtime.ProgramPointWaitForIdle {
			waits = append(waits, c)
		}
	}
	if len(waits) != 2 {
		t.Fatalf("idle waits %+v, want one per nudge", waits)
	}
	for _, w := range waits {
		if w.Program != runtime.ProgramVerdictIdle || w.Status.State != runtime.ProgramIdle {
			t.Fatalf("idle wait %+v judged on a %q reading, want the idle record it decided beside, from before any submit", w, w.Status.State)
		}
	}
}

// orderWriter is a hidden attach client's stdin that logs each write in
// ex's command order, as "hidden-write <input>".
type orderWriter struct{ ex *shadowExecutor }

func (w orderWriter) Write(b []byte) (int, error) {
	w.ex.mu.Lock()
	defer w.ex.mu.Unlock()
	w.ex.order = append(w.ex.order, "hidden-write "+string(b))
	return len(b), nil
}

func (orderWriter) Close() error { return nil }

// Kills a shadowing nudge through a hidden attach client that reads the
// program's record between its look for a pending prompt and the text it
// writes (review of ga-f4ovjm.77: a wait judged there widened the #2892
// window, so a dialog opening during the read took the text as menu
// input). Every program read precedes that look.
func TestShadowHiddenAttachedNudgeReadsBeforeItsLook(t *testing.T) {
	for _, c := range []struct {
		name               string
		ownWait, outerWait bool
	}{
		{"the op lane's wait (production)", false, true},
		{"its own idle wait", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			const session = "shadow-nudge-hidden"
			var order []string
			synctest.Test(t, func(t *testing.T) {
				idle := psRecord(t, readNonce, runtime.ProgramIdle, liveSince)
				inner := &agentPaneExecutor{session: session, provider: "claude", attached: "0", pane: shadowIdleClaudePane}
				tm, ex := shadowTmux(runtime.ProgramStatusShadow, inner, func(int) string { return idle }, newShadowCollector().sink)
				tm.cfg.ComparisonFollowUps = nil
				tm.cfg.DebounceMs = 0
				tm.cfg.NudgeIdleTimeout = 0
				if c.ownWait {
					tm.cfg.NudgeIdleTimeout = 2 * time.Second
				}
				tm.hiddenAttachMu.Lock()
				tm.hiddenAttachClients = map[string]*hiddenAttachClient{session: {stdin: orderWriter{ex: ex}}}
				tm.hiddenAttachMu.Unlock()
				p := &Provider{tm: tm}
				if c.outerWait {
					p.ProgramStatusShadowIdleWait(session, time.Now().Add(-time.Second), time.Now(), runtime.ProgramVerdictIdle)
				}
				if err := p.Nudge(session, runtime.TextContent("hello")); err != nil {
					t.Fatalf("Nudge = %v, want the hidden client's delivery", err)
				}
				synctest.Wait()
				ex.mu.Lock()
				order = slices.Clone(ex.order)
				ex.mu.Unlock()
			})
			write := slices.Index(order, "hidden-write hello")
			if write < 0 {
				t.Fatalf("no text written through the hidden client:\n%q", order)
			}
			look := -1
			for i, c := range order[:write] {
				if strings.Contains(c, "capture-pane") {
					look = i
				}
			}
			if look < 0 {
				t.Fatalf("no look for a pending prompt before the text write:\n%q", order[:write+1])
			}
			if slices.Contains(order[look:write], "program-read") {
				t.Fatalf("a program read came between the last look for a pending prompt and the text write:\n%q", order[look:write+1])
			}
			if reads, _ := readsBefore(order, func(c string) bool { return c == "hidden-write hello" }); reads != 1 {
				t.Fatalf("%d program reads before the text write, want the wait's one:\n%q", reads, order[:write+1])
			}
		})
	}
}
