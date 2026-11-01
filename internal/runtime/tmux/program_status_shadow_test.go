package tmux

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// shadowExecutor answers a shadowing leaf's own reads, the fresh program
// read (programFreshFormat) and the whole-environment read, and hands every
// other command to inner under its lock: inner sees exactly the commands the
// decision path sends, shadow or not.
type shadowExecutor struct {
	mu    sync.Mutex
	inner executor
	// record returns the nth program read's published record.
	record func(n int) string
	// dead reports whether the nth program read finds the pane dead; nil
	// never does.
	dead func(n int) bool
	// tap returns the nth program read's tap nonce; nil reads readNonce.
	tap func(n int) string
	// hold, when set, runs before a program read is answered, outside the
	// lock.
	hold  func()
	reads int
	// order is every command in the order sent, a program read as
	// "program-read".
	order []string
}

// isProgramRead reports whether args is a leaf's fresh program read.
func isProgramRead(args []string) bool {
	return tmuxSubcommand(args) == "list-panes" && args[len(args)-1] == programFreshFormat
}

func (e *shadowExecutor) execute(args []string) (string, error) {
	if e.hold != nil && isProgramRead(args) {
		e.hold()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case isProgramRead(args):
		rec, dead, tap := "", "0", readNonce
		if e.record != nil {
			rec = e.record(e.reads)
		}
		if e.dead != nil && e.dead(e.reads) {
			dead = "1"
		}
		if e.tap != nil {
			tap = e.tap(e.reads)
		}
		e.reads++
		e.order = append(e.order, "program-read")
		return strings.Join([]string{dead, "agent", "%5", "1", tap, rec, runtimeInventoryEnd}, "\t"), nil
	case tmuxSubcommand(args) == "show-environment" && strings.HasPrefix(args[len(args)-1], "="):
		return "GC_PROVIDER=claude\nGC_SESSION_ID=gc-7\nSECRET=x", nil
	}
	e.order = append(e.order, strings.Join(args, " "))
	return e.inner.execute(args)
}

func (e *shadowExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// programReads is how many fresh program reads the leaf took.
func (e *shadowExecutor) programReads() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reads
}

// shadowCollector is a comparison sink that keeps what it is handed.
type shadowCollector struct {
	mu  sync.Mutex
	got []runtime.ProgramStatusComparison
	ch  chan struct{}
}

func newShadowCollector() *shadowCollector { return &shadowCollector{ch: make(chan struct{}, 64)} }

func (c *shadowCollector) sink(cmp runtime.ProgramStatusComparison) {
	c.mu.Lock()
	c.got = append(c.got, cmp)
	c.mu.Unlock()
	c.ch <- struct{}{}
}

// wait returns once n comparisons arrived, failing the test after a bound.
func (c *shadowCollector) wait(t *testing.T, n int) []runtime.ProgramStatusComparison {
	t.Helper()
	return c.waitWithin(t, n, 10*time.Second)
}

// waitWithin is wait bounded by within.
func (c *shadowCollector) waitWithin(t *testing.T, n int, within time.Duration) []runtime.ProgramStatusComparison {
	t.Helper()
	for {
		c.mu.Lock()
		got := slices.Clone(c.got)
		c.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-c.ch:
		case <-time.After(within):
			t.Fatalf("got %d comparisons, want %d: %+v", len(got), n, got)
		}
	}
}

// shadowTmux is a Tmux over inner under mode, its capture gate passed, its
// program reads answered by record, comparisons going to sink, in a process
// that follows timed comparisons up.
func shadowTmux(mode runtime.ProgramStatusMode, inner executor, record func(int) string, sink runtime.ProgramStatusComparisonSink) (*Tmux, *shadowExecutor) {
	ex := &shadowExecutor{inner: inner, record: record}
	cfg := DefaultConfig()
	cfg.ProgramStatus, cfg.ComparisonSink = mode, sink
	cfg.ComparisonFollowUps = func() bool { return true }
	tm := &Tmux{cfg: cfg, exec: ex}
	tm.capture.probe = passGate
	return tm, ex
}

// stubShadowFollow makes follow-ups poll without wall time for the test.
func stubShadowFollow(t *testing.T) {
	t.Helper()
	prev := shadowFollowInterval
	shadowFollowInterval = func(time.Duration) time.Duration { return time.Millisecond }
	t.Cleanup(func() { shadowFollowInterval = prev })
}

// Kills a sink handed out under any switch but shadow, or without a sink.
func TestShadowSinkOnlyUnderShadow(t *testing.T) {
	sink := func(runtime.ProgramStatusComparison) {}
	for _, c := range []struct {
		mode runtime.ProgramStatusMode
		sink runtime.ProgramStatusComparisonSink
		want bool
	}{
		{runtime.ProgramStatusOff, sink, false},
		{runtime.ProgramStatusOn, sink, false},
		{runtime.ProgramStatusShadow, nil, false},
		{runtime.ProgramStatusShadow, sink, true},
	} {
		p := &Provider{tm: &Tmux{cfg: Config{ProgramStatus: c.mode, ComparisonSink: c.sink}}}
		if got := p.ProgramStatusShadowSink() != nil; got != c.want {
			t.Errorf("mode %q, sink %v: sink handed out = %v, want %v", c.mode, c.sink != nil, got, c.want)
		}
	}
}

// Kills a shadow sink that keeps the switch the leaf was built with: a
// reload publishing shadow starts recording, and one publishing off or on
// stops it.
func TestShadowSinkFollowsPublishedSwitch(t *testing.T) {
	sink := func(runtime.ProgramStatusComparison) {}
	for _, built := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusOn} {
		p := &Provider{tm: &Tmux{cfg: Config{ProgramStatus: built, ComparisonSink: sink}}}
		if p.ProgramStatusShadowSink() != nil {
			t.Fatalf("built under %s: a sink is handed out", built)
		}
		p.tm.SetProgramStatusMode(runtime.ProgramStatusShadow)
		if p.ProgramStatusShadowSink() == nil {
			t.Fatalf("built under %s, published shadow: no sink handed out", built)
		}
		p.tm.SetProgramStatusMode(built)
		if p.ProgramStatusShadowSink() != nil {
			t.Fatalf("published %s again: a sink is still handed out", built)
		}
	}
}

// Kills a shadowing idle wait that decides differently from the switch off,
// sends the scan any other command, or records the wrong comparison: the
// scan's verdict beside the record's, the record followed to time its veto.
func TestShadowWaitForIdleDecisionIdenticalToOff(t *testing.T) {
	stubShadowFollow(t)
	quiet := time.Now()
	working := func(t *testing.T) string { return psRecord(t, readNonce, runtime.ProgramWorking, liveSince) }
	ended := func(t *testing.T) string {
		return psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) {
			s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince = 3, 1, 1, quiet
		})
	}
	cases := []struct {
		name          string
		captures      []scriptedCapture
		timeout       time.Duration
		records       func(t *testing.T) []string
		scrape, prog  runtime.ProgramVerdict
		wantTimed     bool
		wantErrIsIdle bool
	}{
		{
			name:     "scan idle, record still working",
			captures: []scriptedCapture{{out: "❯ "}},
			timeout:  2 * time.Second,
			records:  func(t *testing.T) []string { return []string{working(t), working(t), ended(t)} },
			scrape:   runtime.ProgramVerdictIdle, prog: runtime.ProgramVerdictBusy, wantTimed: true,
		},
		{
			name:     "scan busy to the timeout, record idle",
			captures: []scriptedCapture{{out: busyPane}},
			timeout:  500 * time.Millisecond,
			records:  func(t *testing.T) []string { return []string{ended(t)} },
			scrape:   runtime.ProgramVerdictBusy, prog: runtime.ProgramVerdictIdle, wantTimed: true, wantErrIsIdle: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			records := c.records(t)
			record := func(n int) string { return records[min(n, len(records)-1)] }
			offInner := &paneScriptExecutor{captures: c.captures}
			off, offEx := shadowTmux(runtime.ProgramStatusOff, offInner, record, nil)
			offErr := off.WaitForIdle(context.Background(), "s", c.timeout)

			col := newShadowCollector()
			onInner := &paneScriptExecutor{captures: c.captures}
			sh, _ := shadowTmux(runtime.ProgramStatusShadow, onInner, record, col.sink)
			shErr := sh.WaitForIdle(context.Background(), "s", c.timeout)

			if (offErr == nil) != (shErr == nil) || errors.Is(offErr, ErrIdleTimeout) != errors.Is(shErr, ErrIdleTimeout) ||
				errors.Is(offErr, runtime.ErrWaitUnobserved) != errors.Is(shErr, runtime.ErrWaitUnobserved) {
				t.Fatalf("shadow WaitForIdle = %v, off = %v: the decision changed", shErr, offErr)
			}
			if c.wantErrIsIdle != errors.Is(shErr, ErrIdleTimeout) {
				t.Fatalf("WaitForIdle = %v", shErr)
			}
			if offInner.next != onInner.next {
				t.Fatalf("shadow scan took %d captures, off %d", onInner.next, offInner.next)
			}
			if offEx.programReads() != 0 {
				t.Fatalf("the switch off took %d program reads, want none", offEx.programReads())
			}
			got := col.wait(t, 1)[0]
			if got.Point != runtime.ProgramPointWaitForIdle || got.Scrape != c.scrape || got.Program != c.prog || got.Timed != c.wantTimed {
				t.Fatalf("comparison %+v, want %s %s/%s timed %v", got, runtime.ProgramPointWaitForIdle, c.scrape, c.prog, c.wantTimed)
			}
			if got.Session != "s" || got.Family != "claude" || got.SessionID != "gc-7" {
				t.Fatalf("comparison identity %q %q %q, want s claude gc-7", got.Session, got.Family, got.SessionID)
			}
		})
	}
}

// liveSince makes a record's state entered now, so its holds are live.
func liveSince(s *runtime.ProgramStatus) { s.Since = time.Now() }

// Kills a shadowing nudge that types, submits or reports differently from
// the switch off, or that records no typing gate and submit comparison: the
// record read once before the paste is both the gate's reading and the
// baseline the submit is confirmed against.
func TestShadowNudgeDecisionIdenticalToOff(t *testing.T) {
	const session = "shadow-nudge"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	run := func(mode runtime.ProgramStatusMode, sink runtime.ProgramStatusComparisonSink) ([][]string, int, error) {
		var err error
		var calls [][]string
		var reads int
		synctest.Test(t, func(t *testing.T) {
			idle := psRecord(t, readNonce, runtime.ProgramIdle, liveSince)
			started := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })
			inner := &agentPaneExecutor{
				session:  session,
				provider: "claude",
				attached: "0",
				pane:     []string{"● Done.", "────", "❯ [Pasted text #1 +42 lines]", "────", "  ⏵⏵ bypass permissions on"},
				after:    []string{"● Done.", "❯ [Pasted text #1 +42 lines]", "", "* Incubating…", "", "────", "❯ ", "────", "  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt"},
			}
			record := func(n int) string {
				if n == 0 {
					return idle
				}
				return started
			}
			tm, ex := shadowTmux(mode, inner, record, sink)
			err = tm.NudgeSession(session, "reminder: please respond to the review")
			synctest.Wait()
			// The bubble created this session's nudge lock; the next run's
			// bubble must make its own.
			sessionNudgeLocks.Delete(session)
			calls, reads = inner.calls, ex.programReads()
			if mode == runtime.ProgramStatusShadow {
				assertShadowReadBeforeLastLook(t, ex.order)
			}
		})
		return calls, reads, err
	}
	offCalls, offReads, offErr := run(runtime.ProgramStatusOff, nil)
	col := newShadowCollector()
	shCalls, _, shErr := run(runtime.ProgramStatusShadow, col.sink)
	if (offErr == nil) != (shErr == nil) || (offErr != nil && offErr.Error() != shErr.Error()) {
		t.Fatalf("shadow NudgeSession = %v, off = %v: the outcome changed", shErr, offErr)
	}
	if !slices.EqualFunc(offCalls, shCalls, slices.Equal) {
		t.Fatalf("shadow sent the pane other commands than off:\nshadow %q\noff    %q", shCalls, offCalls)
	}
	if offReads != 0 {
		t.Fatalf("the switch off took %d program reads, want none", offReads)
	}
	got := col.wait(t, 2)
	byPoint := map[runtime.ProgramStatusPoint]runtime.ProgramStatusComparison{}
	for _, c := range got {
		byPoint[c.Point] = c
	}
	if g := byPoint[runtime.ProgramPointTypingGate]; g.Scrape != runtime.ProgramVerdictNone || g.Program != runtime.ProgramVerdictNone {
		t.Fatalf("typing gate %+v, want none/none", g)
	}
	// The confirm window saw no busy transition (the turn began after it), so
	// today's scan reads unconfirmed; the record already advanced past the
	// baseline: a record-right disagreement.
	if s := byPoint[runtime.ProgramPointSubmitConfirm]; s.Scrape != runtime.ProgramVerdictUnconfirmed || s.Program != runtime.ProgramVerdictConfirmed {
		t.Fatalf("submit confirm %+v, want unconfirmed/confirmed", s)
	}
}

// Kills a shadowing pending read whose answer differs from the switch off,
// or whose comparison is not the parsers' verdict beside the record's.
func TestShadowPendingDecisionIdenticalToOff(t *testing.T) {
	blocked := psRecord(t, readNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Kind = runtime.ProgramBlockQuestion })
	out := fetchRows(armedPane(blocked))
	table := tapTable(readNonce)
	// The pane shows a ready prompt and no dialog: the parsers read nothing
	// pending, while the record reports a question.
	pendingProvider := func(mode runtime.ProgramStatusMode) *Provider {
		p, _ := readProvider(mode, out, &table)
		p.tm.exec = &pendingPaneExecutor{fetch: out}
		return p
	}

	offPending, offErr := pendingProvider(runtime.ProgramStatusOff).Pending("s")

	sh := pendingProvider(runtime.ProgramStatusShadow)
	col := newShadowCollector()
	sh.tm.cfg.ComparisonSink = col.sink
	shPending, shErr := sh.Pending("s")
	if shErr != nil || offErr != nil || shPending != nil || offPending != nil {
		t.Fatalf("shadow Pending = %v, %v; off = %v, %v; want nil, nil for both", shPending, shErr, offPending, offErr)
	}
	got := col.wait(t, 1)[0]
	if got.Point != runtime.ProgramPointPending || got.Scrape != runtime.ProgramVerdictNone || got.Program != runtime.ProgramVerdictPending || got.Status.Kind != runtime.ProgramBlockQuestion {
		t.Fatalf("comparison %+v, want pending none/pending (question)", got)
	}
}

// Kills wrapped looks (the startup ladder's busy look, the Escape re-send
// look) that answer differently from the look they wrap, or record the wrong
// comparison against the baseline.
func TestShadowWrappedLooksAnswerAsTheirLook(t *testing.T) {
	started := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })
	baseline := runtime.ProgramStatus{Validity: runtime.ProgramStatusValid, State: runtime.ProgramIdle, Nonce: readNonce, Seq: 1}.Baseline()
	lookErr := errors.New("capture failed")
	for _, c := range []struct {
		name  string
		value bool
		err   error
	}{{"true", true, nil}, {"false", false, nil}, {"error", false, lookErr}} {
		t.Run(c.name, func(t *testing.T) {
			col := newShadowCollector()
			tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(int) string { return started }, col.sink)
			look := func() (bool, error) { return c.value, c.err }
			for _, wrapped := range []func() (bool, error){
				tm.shadowStartupBusy("s", baseline, look),
				tm.shadowResendLook("s", baseline, look),
			} {
				if v, err := wrapped(); v != c.value || !errors.Is(err, c.err) {
					t.Fatalf("wrapped look = %v, %v; want %v, %v", v, err, c.value, c.err)
				}
			}
			if c.err != nil {
				return
			}
			got := col.wait(t, 2)
			for _, g := range got {
				switch g.Point {
				case runtime.ProgramPointStartupLadder:
					if g.Program != runtime.ProgramVerdictConfirmed {
						t.Errorf("startup ladder %+v, want program confirmed", g)
					}
				case runtime.ProgramPointEscapeResend:
					if g.Program != runtime.ProgramVerdictNoResend {
						t.Errorf("escape resend %+v, want program no_resend", g)
					}
				default:
					t.Errorf("unexpected comparison %+v", g)
				}
			}
		})
	}
}

// Kills an interrupt settle that is not recorded after an interrupt gc noted,
// is recorded for a wait no interrupt preceded, or files a settle wait's
// timeout as the scrape deciding unsettled when no read judged the turn
// (runtime.ErrWaitUnobserved: a dialog on screen, an unreadable rollout).
// Such a wait decided nothing, whether it was the idle wait after an
// Interrupt or the interrupt-boundary wait.
func TestShadowInterruptSettle(t *testing.T) {
	stubShadowFollow(t)
	working := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
	stopped := psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnEnds, s.QuietSince = 5, 1, time.Now() })
	records := []string{working, stopped}
	// The mark keeps the idle wait's own wait_for_idle out of the record.
	callerRecords := runtime.WithShadowCallerRecords(context.Background())
	idleWait := func(tm *Tmux, err error) {
		tm.shadowIdleWait(callerRecords, "s", time.Now(), time.Now(), err)
	}
	boundaryWait := func(tm *Tmux, err error) {
		mark, ok := tm.takeShadowInterrupt("s", time.Now())
		if !ok {
			t.Fatal("no interrupt noted")
		}
		tm.shadowSettleWait("s", mark, time.Now(), err)
	}
	t.Run("no interrupt noted", func(t *testing.T) {
		col := newShadowCollector()
		tm, ex := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(n int) string { return records[min(n, 1)] }, col.sink)
		idleWait(tm, nil)
		if reads := ex.programReads(); reads != 0 {
			t.Fatalf("a wait no interrupt preceded took %d record reads, want none", reads)
		}
		tm.shadowNoteInterrupt("s")
		idleWait(tm, nil)
		got := col.wait(t, 1)
		if len(got) != 1 || got[0].Point != runtime.ProgramPointInterruptSettle || got[0].Scrape != runtime.ProgramVerdictSettled || got[0].Program != runtime.ProgramVerdictSettled {
			t.Fatalf("comparisons %+v, want one interrupt_settle settled/settled", got)
		}
	})
	for _, wait := range []struct {
		name string
		run  func(tm *Tmux, err error)
	}{{"idle wait", idleWait}, {"interrupt boundary wait", boundaryWait}} {
		for _, c := range []struct {
			name   string
			err    error
			scrape runtime.ProgramVerdict
		}{
			{"returned", nil, runtime.ProgramVerdictSettled},
			{"timed out busy", ErrIdleTimeout, runtime.ProgramVerdictUnsettled},
			{"timed out unread", idleTimeout(false), runtime.ProgramVerdictIndeterminate},
		} {
			t.Run(wait.name+": "+c.name, func(t *testing.T) {
				col := newShadowCollector()
				tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(n int) string { return records[min(n, 1)] }, col.sink)
				tm.shadowNoteInterrupt("s")
				wait.run(tm, c.err)
				got := col.wait(t, 1)[0]
				if got.Point != runtime.ProgramPointInterruptSettle || got.Scrape != c.scrape || got.Program != runtime.ProgramVerdictSettled {
					t.Fatalf("comparison %+v, want interrupt_settle %s/settled", got, c.scrape)
				}
			})
		}
	}
}

// Kills background shadow work that blocks when every slot is taken: the
// work is dropped, and the drop rides on the next comparison.
func TestShadowGoDropsWhenFull(t *testing.T) {
	col := newShadowCollector()
	tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, nil, col.sink)
	release := make(chan struct{})
	for range shadowSlots {
		tm.shadowGo(func() { <-release })
	}
	done := make(chan struct{})
	go func() {
		tm.shadowGo(func() { t.Error("ran past a full set of slots") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shadowGo blocked on full slots")
	}
	close(release)
	tm.shadowEmit("s", runtime.ProgramPointPending, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone, runtime.ProgramStatus{}, time.Now(), 0, false)
	if got := col.wait(t, 1)[0]; got.Dropped != 1 {
		t.Fatalf("next comparison Dropped = %d, want 1", got.Dropped)
	}
}

// pendingPaneExecutor resolves the agent pane like fakeAgentPaneRow, shows a
// ready prompt to every capture, answers the state cache's listing with
// fetch, and every other command with "".
type pendingPaneExecutor struct{ fetch string }

func (e *pendingPaneExecutor) execute(args []string) (string, error) {
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "capture-pane":
		return "❯ ", nil
	case "list-panes":
		return e.fetch, nil
	}
	return "", nil
}

func (e *pendingPaneExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// Kills a readiness comparison that ignores the settle after the program's
// first report, records a wait its context ended, stops following a capture
// that is still silent (so a program that reports after the scrape decided is
// never timed), or follows a capture that will not report.
func TestShadowReadiness(t *testing.T) {
	stubShadowFollow(t)
	t.Run("reported before the decision", func(t *testing.T) {
		reported := psRecord(t, readNonce, runtime.ProgramIdle, func(s *runtime.ProgramStatus) { s.Since = time.Now().Add(-2 * time.Second) })
		col := newShadowCollector()
		tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(int) string { return reported }, col.sink)
		tm.shadowReadiness("s", time.Now(), context.Canceled)
		tm.shadowReadiness("s", time.Now(), nil)
		got := col.wait(t, 1)
		if len(got) != 1 || got[0].Point != runtime.ProgramPointReadiness || got[0].Scrape != runtime.ProgramVerdictReady ||
			got[0].Program != runtime.ProgramVerdictReady || !got[0].Timed || got[0].LatencyMs >= 0 {
			t.Fatalf("comparisons %+v, want one readiness ready/ready, timed, the record earlier", got)
		}
	})
	t.Run("first report after the decision", func(t *testing.T) {
		decided := time.Now().Truncate(time.Millisecond)
		first := psRecord(t, readNonce, runtime.ProgramIdle, func(s *runtime.ProgramStatus) { s.Since = decided.Add(time.Second) })
		// Silent, silent, then the program's first report.
		records := []string{"", "", first}
		col := newShadowCollector()
		tm, ex := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(n int) string { return records[min(n, 2)] }, col.sink)
		tm.shadowReadiness("s", decided, nil)
		got := col.wait(t, 1)[0]
		if got.Scrape != runtime.ProgramVerdictReady || got.Program != runtime.ProgramVerdictNotReady || got.Status.Validity != runtime.ProgramStatusSilent {
			t.Fatalf("comparison %+v, want ready/not_ready on the silent reading judged at the decision", got)
		}
		want := (time.Second + runtime.ProgramStatusReadySettle).Milliseconds()
		if !got.Timed || got.LatencyMs != want {
			t.Fatalf("comparison timed %v latency %d ms, want timed %d ms (first report plus settle after the decision)", got.Timed, got.LatencyMs, want)
		}
		if reads := ex.programReads(); reads != 3 {
			t.Fatalf("took %d program reads, want 3: through both silent readings to the report", reads)
		}
	})
	t.Run("capture ends before a report", func(t *testing.T) {
		col := newShadowCollector()
		tm, ex := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(int) string { return "" }, col.sink)
		ex.dead = func(n int) bool { return n >= 1 }
		tm.shadowReadiness("s", time.Now(), nil)
		got := col.wait(t, 1)[0]
		if got.Timed || got.Status.Validity != runtime.ProgramStatusSilent {
			t.Fatalf("comparison %+v, want untimed on the silent reading judged at the decision", got)
		}
		if reads := ex.programReads(); reads != 2 {
			t.Fatalf("took %d program reads, want 2: the follow-up ends at the exited reading", reads)
		}
	})
}

// Kills a comparison that carries a follow-up's last reading instead of the
// reading its rule judged at the decision, or that is not judged at the time
// that reading was taken, before the point returned: a record cleared or lost
// during the follow-up would turn a valid disagreement into capture noise,
// one that moved on would pair the verdict with a state it was not taken
// from, and the hold liveness and record age recorded beside it would be the
// follow-up's, minutes after the decision.
func TestShadowComparisonCarriesDecisionReading(t *testing.T) {
	stubShadowFollow(t)
	working := func(t *testing.T) string { return psRecord(t, readNonce, runtime.ProgramWorking, liveSince) }
	ended := func(t *testing.T) string {
		return psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) {
			s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince = 3, 1, 1, time.Now()
		})
	}
	cleared := func(t *testing.T) string {
		return psRecord(t, readNonce, "", func(s *runtime.ProgramStatus) { s.Seq, s.Reported = 3, true })
	}
	idle := runtime.ProgramStatus{Validity: runtime.ProgramStatusValid, State: runtime.ProgramIdle, Nonce: readNonce, Seq: 1}
	cases := []struct {
		name      string
		records   func(t *testing.T) []string
		run       func(tm *Tmux, decided time.Time)
		program   runtime.ProgramVerdict
		state     runtime.ProgramState
		wantTimed bool
	}{
		{
			name:    "wait_for_idle: working, then cleared",
			records: func(t *testing.T) []string { return []string{working(t), cleared(t)} },
			run: func(tm *Tmux, decided time.Time) {
				tm.shadowIdleWait(context.Background(), "s", decided, decided, nil)
			},
			program: runtime.ProgramVerdictBusy, state: runtime.ProgramWorking,
		},
		{
			name:    "wait_for_idle: working, then idle",
			records: func(t *testing.T) []string { return []string{working(t), ended(t)} },
			run: func(tm *Tmux, decided time.Time) {
				tm.shadowIdleWait(context.Background(), "s", decided, decided, nil)
			},
			program: runtime.ProgramVerdictBusy, state: runtime.ProgramWorking, wantTimed: true,
		},
		{
			name: "submit_confirm: not advanced, then cleared",
			records: func(t *testing.T) []string {
				return []string{psRecord(t, readNonce, runtime.ProgramIdle, liveSince), cleared(t)}
			},
			run: func(tm *Tmux, decided time.Time) {
				tm.shadowSubmit("s", idle.Baseline(), runtime.ProgramVerdictConfirmed, decided)
			},
			program: runtime.ProgramVerdictUnconfirmed, state: runtime.ProgramIdle,
		},
		{
			name:    "interrupt_settle: working, then settled",
			records: func(t *testing.T) []string { return []string{working(t), ended(t)} },
			run: func(tm *Tmux, decided time.Time) {
				mark := shadowInterrupt{baseline: runtime.ProgramStatus{Validity: runtime.ProgramStatusValid, State: runtime.ProgramWorking, Nonce: readNonce, Seq: 1}.Baseline(), at: decided}
				tm.shadowSettleWait("s", mark, decided, nil)
			},
			program: runtime.ProgramVerdictUnsettled, state: runtime.ProgramWorking, wantTimed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			records := c.records(t)
			col := newShadowCollector()
			tm, ex := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(n int) string { return records[min(n, len(records)-1)] }, col.sink)
			decided := time.Now()
			c.run(tm, decided)
			returned := time.Now()
			got := col.wait(t, 1)[0]
			if got.Program != c.program || got.Timed != c.wantTimed {
				t.Fatalf("comparison %+v, want program %s timed %v", got, c.program, c.wantTimed)
			}
			if got.Status.Validity != runtime.ProgramStatusValid || got.Status.State != c.state {
				t.Fatalf("comparison carries a %q %q reading, want the valid %q one judged at the decision", got.Status.Validity, got.Status.State, c.state)
			}
			if got.JudgedAt.Before(decided) || got.JudgedAt.After(returned) {
				t.Fatalf("comparison judged at %v, want its decision reading's time, between the decision %v and the point's return %v", got.JudgedAt, decided, returned)
			}
			if reads := ex.programReads(); reads != 2 {
				t.Fatalf("took %d program reads, want 2: the decision's and one follow-up read", reads)
			}
		})
	}
}

// Kills a follow-up that misses a boundary shorter than its poll: gc types
// its delivery as soon as the scrape reads idle, so the program's boundary
// lasts only until that delivery starts its next turn, and the follow-up
// would time the delivered turn's end instead. A turn that ended since the
// decision is the boundary, timed at its QuietSince when it is the only one;
// with more the first one's time is gone, and a boundary of another
// incarnation is not this decision's at all.
func TestShadowFollowUpTimesTheFirstBoundary(t *testing.T) {
	stubShadowFollow(t)
	const respawnNonce = "r3spawn.%5"
	decided := time.Now().Truncate(time.Millisecond)
	ended := decided.Add(300 * time.Millisecond) // the boundary the delivery left at once
	later := decided.Add(5 * time.Second)        // the delivered turn's own end
	working := func(t *testing.T) string {
		return psRecord(t, readNonce, runtime.ProgramWorking, func(s *runtime.ProgramStatus) { s.Since = decided })
	}
	// resumed is the record back at work after turns ended since the
	// decision, the last of them at quiet.
	resumed := func(t *testing.T, turns uint32, quiet time.Time) string {
		return psRecord(t, readNonce, runtime.ProgramWorking, func(s *runtime.ProgramStatus) {
			s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince, s.Since = 2+2*uint64(turns), 1+turns, turns, quiet, quiet
		})
	}
	quietAfter := func(t *testing.T, turns uint32, quiet time.Time) string {
		return psRecord(t, readNonce, runtime.ProgramIdle, func(s *runtime.ProgramStatus) {
			s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince, s.Since = 2+2*uint64(turns), turns, turns, quiet, quiet
		})
	}
	wait := func(tm *Tmux) { tm.shadowIdleWait(context.Background(), "s", decided.Add(-time.Second), decided, nil) }
	settle := func(tm *Tmux) {
		baseline := runtime.ProgramStatus{Validity: runtime.ProgramStatusValid, State: runtime.ProgramWorking, Nonce: readNonce, Seq: 1}.Baseline()
		tm.shadowSettleWait("s", shadowInterrupt{baseline: baseline, at: decided.Add(-time.Second)}, decided, nil)
	}
	cases := []struct {
		name      string
		records   func(t *testing.T) []string
		tap       func(n int) string
		run       func(tm *Tmux)
		wantTimed bool
		wantMs    int64
	}{
		{
			name: "wait_for_idle: one turn ended, the next started",
			records: func(t *testing.T) []string {
				return []string{working(t), resumed(t, 1, ended), quietAfter(t, 2, later)}
			},
			run: wait, wantTimed: true, wantMs: 300,
		},
		{
			name: "interrupt_settle: one turn ended, the next started",
			records: func(t *testing.T) []string {
				return []string{working(t), resumed(t, 1, ended), quietAfter(t, 2, later)}
			},
			run: settle, wantTimed: true, wantMs: 300,
		},
		{
			name: "wait_for_idle: two turns ended",
			records: func(t *testing.T) []string {
				return []string{working(t), resumed(t, 2, later), quietAfter(t, 3, later)}
			},
			run: wait,
		},
		{
			name: "wait_for_idle: the next incarnation's boundary",
			records: func(t *testing.T) []string {
				return []string{working(t), psRecord(t, respawnNonce, runtime.ProgramIdle, func(s *runtime.ProgramStatus) { s.Since = ended })}
			},
			tap: func(n int) string {
				if n == 0 {
					return readNonce
				}
				return respawnNonce
			},
			run: wait,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			records := c.records(t)
			col := newShadowCollector()
			tm, ex := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(n int) string { return records[min(n, len(records)-1)] }, col.sink)
			ex.tap = c.tap
			c.run(tm)
			got := col.wait(t, 1)[0]
			if got.Timed != c.wantTimed || (c.wantTimed && got.LatencyMs != c.wantMs) {
				t.Fatalf("comparison timed %v latency %d ms, want timed %v latency %d ms", got.Timed, got.LatencyMs, c.wantTimed, c.wantMs)
			}
			if reads := ex.programReads(); reads != 2 {
				t.Fatalf("took %d program reads, want 2: the follow-up ends at the first reading past the decision's turn", reads)
			}
		})
	}
}

// assertShadowReadBeforeLastLook fails unless a nudge's record read comes
// before the last prompt look ahead of the paste: a read between that look
// and the paste would widen the window a prompt can appear in unseen.
func assertShadowReadBeforeLastLook(t *testing.T, order []string) {
	t.Helper()
	paste := slices.IndexFunc(order, func(c string) bool {
		return strings.Contains(c, "send-keys") && strings.Contains(c, "-l")
	})
	if paste < 0 {
		t.Fatalf("no paste in %q", order)
	}
	lastLook, read := -1, -1
	for i, c := range order[:paste] {
		switch {
		case c == "program-read":
			read = i
		case strings.Contains(c, "capture-pane"):
			lastLook = i
		}
	}
	if read < 0 || lastLook < 0 || read > lastLook {
		t.Fatalf("record read at %d, last prompt look at %d, paste at %d: the read must precede the look\n%q", read, lastLook, paste, order[:paste+1])
	}
}

// bigShadowCollector is a shadowCollector that holds n wake-ups unread.
func bigShadowCollector(n int) *shadowCollector { return &shadowCollector{ch: make(chan struct{}, n)} }

// idleProofsPerHour is the maintainer-city rate of failed idle proofs on a
// busy fleet (R1B review M1).
const idleProofsPerHour = 400

// Kills an idle proof that records a tmux-level wait_for_idle beside its own
// proved_idle: an hour of failed proofs on a busy seat records nothing at
// the leaf, takes no record read and holds no background slot.
func TestShadowIdleProofRecordsNoWaitForIdle(t *testing.T) {
	// Each failed wait sleeps one idle poll; the bubble's clock makes an
	// hour of them take no wall time.
	synctest.Test(t, func(t *testing.T) {
		col := bigShadowCollector(idleProofsPerHour)
		working := psRecord(t, readNonce, runtime.ProgramWorking, liveSince)
		tm, ex := shadowTmux(runtime.ProgramStatusShadow, &paneScriptExecutor{captures: []scriptedCapture{{out: busyPane}}},
			func(int) string { return working }, col.sink)
		ctx := runtime.WithShadowCallerRecords(context.Background())
		for range idleProofsPerHour {
			if err := tm.WaitForIdle(ctx, "s", time.Millisecond); !errors.Is(err, ErrIdleTimeout) {
				t.Fatalf("WaitForIdle = %v, want the busy scan's timeout", err)
			}
		}
		synctest.Wait()
		if n, reads, held := len(col.got), ex.programReads(), len(tm.shadow.slots); n != 0 || reads != 0 || held != 0 {
			t.Fatalf("an hour of idle proofs recorded %d comparisons, took %d record reads, holds %d slots; want none", n, reads, held)
		}
		// One unmarked wait (no follow-up, so it ends) shows the leaf was
		// shadowing all along.
		tm.cfg.ComparisonFollowUps = nil
		_ = tm.WaitForIdle(context.Background(), "s", time.Millisecond)
		synctest.Wait()
		if len(col.got) != 1 || col.got[0].Point != runtime.ProgramPointWaitForIdle {
			t.Fatalf("the unmarked wait recorded %+v, want one wait_for_idle", col.got)
		}
	})
}

// Kills a timed point that follows every decision up: with its session busy,
// an hour of idle waits at one session holds one follow-up, records every
// other decision untimed at once, drops nothing and leaves the slots free.
func TestShadowFollowUpOnePerSessionPoint(t *testing.T) {
	stubShadowFollow(t)
	synctest.Test(t, func(t *testing.T) {
		col := bigShadowCollector(idleProofsPerHour)
		var ended atomic.Bool
		working := psRecord(t, readNonce, runtime.ProgramWorking, liveSince)
		quiet := psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) {
			s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince = 3, 1, 1, time.Now()
		})
		tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(int) string {
			if ended.Load() {
				return quiet
			}
			return working
		}, col.sink)
		following := func() int {
			tm.shadow.mu.Lock()
			defer tm.shadow.mu.Unlock()
			return len(tm.shadow.following)
		}
		now := time.Now()
		tm.shadowIdleWait(context.Background(), "s", now, now, nil)
		synctest.Wait()
		if following() != 1 {
			t.Fatal("the first wait started no follow-up")
		}
		for i := 1; i < idleProofsPerHour; i++ {
			tm.shadowIdleWait(context.Background(), "s", now, time.Now(), nil)
			col.wait(t, i)
		}
		synctest.Wait()
		if f, held := following(), len(tm.shadow.slots); f != 1 || held != 1 {
			t.Fatalf("%d follow-ups outstanding holding %d slots, want one of each", f, held)
		}
		for _, c := range col.wait(t, idleProofsPerHour-1) {
			if c.Timed || c.Dropped != 0 || c.Program != runtime.ProgramVerdictBusy {
				t.Fatalf("comparison %+v recorded beside an outstanding follow-up, want untimed busy, nothing dropped", c)
			}
		}
		ended.Store(true)
		last := col.wait(t, idleProofsPerHour)[idleProofsPerHour-1]
		if !last.Timed || last.Program != runtime.ProgramVerdictBusy {
			t.Fatalf("the followed wait recorded %+v, want timed busy", last)
		}
		synctest.Wait()
		if f, held := following(), len(tm.shadow.slots); f != 0 || held != 0 {
			t.Fatalf("follow-up not released: %d outstanding, %d slots held", f, held)
		}
	})
}

// workSignal is a Config.ComparisonWork hook counting begun work and
// signaling each end on ended.
func workSignal(begun *atomic.Int64, ended chan<- struct{}) func() func() {
	return func() func() {
		begun.Add(1)
		return func() { ended <- struct{}{} }
	}
}

// Kills a short-lived process that follows a timed comparison past its
// decision, where its exit would lose it uncounted: without follow-ups the
// decision-time verdict is recorded untimed after one record read, as work
// the process is told of.
func TestShadowNoFollowUpInShortLivedProcess(t *testing.T) {
	stubShadowFollow(t)
	col := newShadowCollector()
	working := psRecord(t, readNonce, runtime.ProgramWorking, liveSince)
	tm, ex := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(int) string { return working }, col.sink)
	tm.cfg.ComparisonFollowUps = nil
	var begun atomic.Int64
	ended := make(chan struct{}, 1)
	tm.cfg.ComparisonWork = workSignal(&begun, ended)
	now := time.Now()
	tm.shadowIdleWait(context.Background(), "s", now, now, nil)
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the process was never told the shadow work ended")
	}
	got := col.wait(t, 1)[0]
	if got.Timed || got.Program != runtime.ProgramVerdictBusy || got.Scrape != runtime.ProgramVerdictIdle {
		t.Fatalf("comparison %+v, want idle/busy untimed", got)
	}
	if reads := ex.programReads(); reads != 1 {
		t.Fatalf("%d record reads, want the one judging read", reads)
	}
	if begun.Load() != 1 {
		t.Fatalf("work begun %d times, want 1", begun.Load())
	}
}

// Kills background shadow work whose panic escapes: the goroutine recovers,
// frees its slot, and counts the lost comparison as dropped.
func TestShadowGoRecoversPanic(t *testing.T) {
	col := newShadowCollector()
	tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, nil, col.sink)
	var begun atomic.Int64
	ended := make(chan struct{}, 1)
	tm.cfg.ComparisonWork = workSignal(&begun, ended)
	tm.shadowGo(func() { panic("shadow bug") })
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("a panicking shadow goroutine never ended its work")
	}
	if held := len(tm.shadow.slots); held != 0 {
		t.Fatalf("a panicking shadow goroutine kept its slot (%d held)", held)
	}
	tm.shadowEmit("s", runtime.ProgramPointPending, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone, runtime.ProgramStatus{}, time.Now(), 0, false)
	if got := col.wait(t, 1)[0]; got.Dropped != 1 {
		t.Fatalf("next comparison Dropped = %d, want the panicked one", got.Dropped)
	}
}

// actGate stands for what a caller does with a decision as soon as the
// decision point hands control back to it: a delivery typed when a wait
// returns, a re-send after a look, the start's dialogs and nudge once
// readiness decided. A read held at it (hold) waits for the act, for up to a
// second: inside a synctest bubble that second passes only once every
// goroutine is blocked, so a read the decision path takes while its caller
// waits on it answers before the act, and a read left to a background
// goroutine answers only after it.
type actGate struct{ acted chan struct{} }

func newActGate() *actGate { return &actGate{acted: make(chan struct{})} }

// act is the caller acting on the decision.
func (g *actGate) act() { close(g.acted) }

// done reports whether the caller acted.
func (g *actGate) done() bool {
	select {
	case <-g.acted:
		return true
	default:
		return false
	}
}

// hold waits for the act, up to a second.
func (g *actGate) hold() {
	select {
	case <-g.acted:
	case <-time.After(time.Second):
	}
}

// Kills a decision point whose rule judges a reading taken after the point
// handed control back to its caller (review finding: asynchronous decision
// reading). gc acts on a decision at once, and a turn its act starts is no
// evidence of what the program did at the decision: a reading left to a
// background goroutine judged that turn as a disagreement, and the timing
// follow-up started from it. Each point runs here as its decision path runs
// it, the caller acts as soon as it returns, and every program read before
// the act is held back until it: the comparison must carry the reading from
// before the act, and a follow-up still times what the program did after it.
// The comparison is awaited for as long as the longest follow-up may run, so
// a reading judged after the act fails on its comparison rather than leaving
// a follow-up running.
func TestShadowDecisionReadingPrecedesTheCallersAct(t *testing.T) {
	idleBase := runtime.ProgramStatus{Validity: runtime.ProgramStatusValid, State: runtime.ProgramIdle, Nonce: readNonce, Seq: 1}.Baseline()
	callerRecords := runtime.WithShadowCallerRecords(context.Background())
	idleWait := func(ctx context.Context) func(t *testing.T, tm *Tmux) {
		return func(t *testing.T, tm *Tmux) {
			if err := tm.WaitForIdle(ctx, "s", 2*time.Second); err != nil {
				t.Fatalf("WaitForIdle = %v, want the idle prompt's nil", err)
			}
		}
	}
	cases := []struct {
		name string
		// interrupted notes an interrupt first, the first program read its
		// baseline.
		interrupted     bool
		decide          func(t *testing.T, tm *Tmux)
		point           runtime.ProgramStatusPoint
		scrape, program runtime.ProgramVerdict
		timed           bool
	}{
		{
			name:   "wait_for_idle, a delivery typed when the wait returns",
			decide: idleWait(context.Background()),
			point:  runtime.ProgramPointWaitForIdle, scrape: runtime.ProgramVerdictIdle, program: runtime.ProgramVerdictIdle, timed: true,
		},
		{
			name:        "interrupt_settle, a delivery typed when the idle wait after an interrupt returns",
			interrupted: true,
			decide:      idleWait(callerRecords),
			point:       runtime.ProgramPointInterruptSettle, scrape: runtime.ProgramVerdictSettled, program: runtime.ProgramVerdictSettled, timed: true,
		},
		{
			name:        "interrupt_settle, a delivery typed when the interrupt boundary wait returns",
			interrupted: true,
			decide: func(t *testing.T, tm *Tmux) {
				mark, ok := tm.takeShadowInterrupt("s", time.Now())
				if !ok {
					t.Fatal("no interrupt noted")
				}
				tm.shadowSettleWait("s", mark, time.Now(), nil)
			},
			point: runtime.ProgramPointInterruptSettle, scrape: runtime.ProgramVerdictSettled, program: runtime.ProgramVerdictSettled, timed: true,
		},
		{
			// The act is the program taking the submit up: the follow-up
			// times it.
			name: "submit_confirm, the submit taken up after the confirm decided",
			decide: func(_ *testing.T, tm *Tmux) {
				tm.shadowSubmit("s", idleBase, runtime.ProgramVerdictUnconfirmed, time.Now())
			},
			point: runtime.ProgramPointSubmitConfirm, scrape: runtime.ProgramVerdictUnconfirmed, program: runtime.ProgramVerdictUnconfirmed, timed: true,
		},
		{
			name: "escape_resend, the re-send a waiting look decided",
			decide: func(t *testing.T, tm *Tmux) {
				look := tm.shadowResendLook("s", idleBase, func() (bool, error) { return true, nil })
				if waiting, err := look(); !waiting || err != nil {
					t.Fatalf("look = %v, %v; want the wrapped look's true", waiting, err)
				}
			},
			point: runtime.ProgramPointEscapeResend, scrape: runtime.ProgramVerdictResend, program: runtime.ProgramVerdictResend,
		},
		{
			name:   "readiness, the start's nudge typed once readiness decided",
			decide: func(_ *testing.T, tm *Tmux) { tm.shadowReadiness("s", time.Now(), nil) },
			point:  runtime.ProgramPointReadiness, scrape: runtime.ProgramVerdictReady, program: runtime.ProgramVerdictReady, timed: true,
		},
		{
			name: "startup_ladder, the nudge re-sent after a look read no busy pane",
			decide: func(t *testing.T, tm *Tmux) {
				look := tm.shadowStartupBusy("s", idleBase, func() (bool, error) { return false, nil })
				if busy, err := look(); busy || err != nil {
					t.Fatalf("look = %v, %v; want the wrapped look's false", busy, err)
				}
			},
			point: runtime.ProgramPointStartupLadder, scrape: runtime.ProgramVerdictUnconfirmed, program: runtime.ProgramVerdictUnconfirmed,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Uninterrupted: at a boundary since its first report 2 s
				// ago. Interrupted: a turn running when gc interrupted it,
				// stopped at the decision. Either way gc's act starts a turn.
				before := psRecord(t, readNonce, runtime.ProgramIdle, func(s *runtime.ProgramStatus) { s.Since = time.Now().Add(-2 * time.Second) })
				after := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 2, 1 })
				baseline := ""
				if c.interrupted {
					baseline = psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
					before = psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) {
						s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince = 5, 1, 1, time.Now()
					})
					after = psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts, s.TurnEnds = 6, 2, 1 })
				}
				gate := newActGate()
				record := func(n int) string {
					switch {
					case gate.done():
						return after
					case n == 0 && c.interrupted:
						return baseline
					default:
						return before
					}
				}
				col := newShadowCollector()
				tm, ex := shadowTmux(runtime.ProgramStatusShadow, &paneScriptExecutor{captures: []scriptedCapture{{out: "❯ "}}}, record, col.sink)
				ex.hold = gate.hold
				if c.interrupted {
					tm.shadowNoteInterrupt("s")
				}
				c.decide(t, tm)
				gate.act()
				got := col.waitWithin(t, 1, runtime.ProgramStatusHoldLimit+time.Minute)[0]
				synctest.Wait()
				if len(col.got) != 1 {
					t.Fatalf("comparisons %+v, want one", col.got)
				}
				if got.Point != c.point || got.Scrape != c.scrape || got.Program != c.program || got.Timed != c.timed {
					t.Fatalf("comparison %+v, want %s %s/%s timed %v", got, c.point, c.scrape, c.program, c.timed)
				}
				if got.Status.State != runtime.ProgramIdle {
					t.Fatalf("comparison carries a %q reading, want the idle one from before gc acted", got.Status.State)
				}
				if got.Status.Since.After(got.JudgedAt) {
					t.Fatalf("comparison judged at %v, before its reading's state was entered at %v", got.JudgedAt, got.Status.Since)
				}
			})
		})
	}
}

// heldListingExecutor is pendingPaneExecutor whose state cache listing is
// listing(), answered after hold.
type heldListingExecutor struct {
	hold    func()
	listing func() string
}

func (e *heldListingExecutor) execute(args []string) (string, error) {
	if row, ok := fakeAgentPaneRow(args); ok {
		return row, nil
	}
	switch tmuxSubcommand(args) {
	case "capture-pane":
		return "❯ ", nil
	case "list-panes":
		e.hold()
		return e.listing(), nil
	}
	return "", nil
}

func (e *heldListingExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// Kills a pending comparison whose state cache reading is taken after
// Pending returned (review finding: asynchronous decision reading): a
// delivery made once Pending reads nothing pending starts a turn, and a
// reading of the listing after it judged that turn. The listing from before
// the delivery is held back until it, as in
// TestShadowDecisionReadingPrecedesTheCallersAct.
func TestShadowPendingReadingPrecedesTheCallersAct(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := newActGate()
		blocked := psRecord(t, readNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Kind = runtime.ProgramBlockQuestion })
		delivered := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnStarts = 2, 2 })
		ex := &heldListingExecutor{hold: gate.hold, listing: func() string {
			if gate.done() {
				return fetchRows(armedPane(delivered))
			}
			return fetchRows(armedPane(blocked))
		}}
		col := newShadowCollector()
		tm := &Tmux{cfg: Config{ProgramStatus: runtime.ProgramStatusShadow, ComparisonSink: col.sink}, exec: ex}
		tm.capture.probe = passGate
		table := tapTable(readNonce)
		p := &Provider{tm: tm, cfg: tm.cfg, workDirs: map[string]string{}}
		p.cache = NewStateCache(&tmuxFetcher{tm: tm, snapshot: func(context.Context) (processSnapshot, error) { return table, nil }}, time.Hour)
		if pending, err := p.Pending("s"); pending != nil || err != nil {
			t.Fatalf("Pending = %v, %v; want the parsers' nil, nil", pending, err)
		}
		gate.act()
		synctest.Wait()
		if len(col.got) != 1 {
			t.Fatalf("comparisons %+v, want one", col.got)
		}
		if got := col.got[0]; got.Point != runtime.ProgramPointPending || got.Scrape != runtime.ProgramVerdictNone || got.Program != runtime.ProgramVerdictPending || got.Status.State != runtime.ProgramBlocked {
			t.Fatalf("comparison %+v, want pending none/pending on the blocked reading from before the delivery", got)
		}
	})
}

// listingExecutor answers the state cache's listing with session "s"'s armed
// agent pane carrying the record it returns, and nothing else.
type listingExecutor func() string

func (record listingExecutor) execute(args []string) (string, error) {
	if tmuxSubcommand(args) == "list-panes" && args[len(args)-1] == programStateFetchFormat {
		return fetchRows(armedPane(record())), nil
	}
	return "", errors.New("unscripted")
}

func (record listingExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return record.execute(args)
}

// Kills a sleep proof (proved_idle) whose two readings are one cached
// snapshot (review finding: proved_idle on cached readings). The state cache
// answers a session's reading from one listing for its whole TTL, longer
// than a proof takes, so two cached reads around a proof inside which a turn
// ran prove the program quiescent. Through the tmux provider and its state
// cache, with the record moving inside the cache's TTL, the proof's two
// readings are taken fresh from the session: the turn shows and fails the
// record's proof, and with no turn the proof holds.
func TestShadowIdleProofTakesTwoObservations(t *testing.T) {
	quiet := psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) {
		s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince = 3, 1, 1, time.Now()
	})
	ranAgain := psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) {
		s.Seq, s.TurnStarts, s.TurnEnds, s.QuietSince = 5, 2, 2, time.Now()
	})
	for _, c := range []struct {
		name    string
		after   string
		program runtime.ProgramVerdict
	}{
		{"a turn ran inside the proof", ranAgain, runtime.ProgramVerdictBusy},
		{"nothing changed", quiet, runtime.ProgramVerdictIdle},
	} {
		t.Run(c.name, func(t *testing.T) {
			var record atomic.Pointer[string]
			record.Store(&quiet)
			current := func() string { return *record.Load() }
			col := newShadowCollector()
			tm, ex := shadowTmux(runtime.ProgramStatusShadow, listingExecutor(current), func(int) string { return current() }, col.sink)
			table := tapTable(readNonce)
			p := &Provider{tm: tm, cfg: tm.cfg, workDirs: map[string]string{}}
			// One listing serves the whole test: the record moves inside
			// the cache's TTL.
			p.cache = NewStateCache(&tmuxFetcher{tm: tm, snapshot: func(context.Context) (processSnapshot, error) { return table, nil }}, time.Hour)
			tm.shadow.taps = func() programTaps { return p.cache.observation().state.programTaps() }
			if s, err := p.ProgramStatus("s"); err != nil || s.Seq != 3 {
				t.Fatalf("cached reading %+v, %v; want the quiet record", s, err)
			}
			proved := runtime.ShadowIdleProof(context.Background(), p, "s", func(context.Context) bool {
				record.Store(&c.after)
				return true
			})
			if !proved {
				t.Fatal("ShadowIdleProof returned a verdict other than the proof's")
			}
			if s, err := p.ProgramStatus("s"); err != nil || s.Seq != 3 {
				t.Fatalf("cached reading %+v, %v after the proof; want the listing from before it, which two cached reads would both have judged", s, err)
			}
			got := col.wait(t, 1)[0]
			if got.Point != runtime.ProgramPointProvedIdle || got.Scrape != runtime.ProgramVerdictIdle || got.Program != c.program {
				t.Fatalf("comparison %+v, want proved_idle idle/%s", got, c.program)
			}
			if !got.Status.ReaderVerified {
				t.Fatal("the proof's reading did not take its tap's liveness from the cache's process table")
			}
			if reads := ex.programReads(); reads != 2 {
				t.Fatalf("%d fresh reads, want one on each side of the proof", reads)
			}
		})
	}
}
