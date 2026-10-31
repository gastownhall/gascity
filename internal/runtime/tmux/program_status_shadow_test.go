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
	reads  int
	// order is every command in the order sent, a program read as
	// "program-read".
	order []string
}

func (e *shadowExecutor) execute(args []string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case tmuxSubcommand(args) == "list-panes" && args[len(args)-1] == programFreshFormat:
		rec := ""
		if e.record != nil {
			rec = e.record(e.reads)
		}
		e.reads++
		e.order = append(e.order, "program-read")
		return strings.Join([]string{"0", "agent", "%5", "1", readNonce, rec, runtimeInventoryEnd}, "\t"), nil
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
	for {
		c.mu.Lock()
		got := slices.Clone(c.got)
		c.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-c.ch:
		case <-time.After(10 * time.Second):
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
// or is recorded for a wait no interrupt preceded.
func TestShadowInterruptSettle(t *testing.T) {
	stubShadowFollow(t)
	working := psRecord(t, readNonce, runtime.ProgramWorking, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 4 })
	stopped := psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq, s.TurnEnds, s.QuietSince = 5, 1, time.Now() })
	col := newShadowCollector()
	records := []string{working, stopped}
	tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, func(n int) string { return records[min(n, 1)] }, col.sink)
	tm.shadowSettle("s", time.Now(), nil)
	tm.shadowNoteInterrupt("s")
	tm.shadowSettle("s", time.Now(), nil)
	got := col.wait(t, 1)
	if len(got) != 1 || got[0].Point != runtime.ProgramPointInterruptSettle || got[0].Scrape != runtime.ProgramVerdictSettled || got[0].Program != runtime.ProgramVerdictSettled {
		t.Fatalf("comparisons %+v, want one interrupt_settle settled/settled", got)
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
	tm.shadowEmit("s", runtime.ProgramPointPending, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone, runtime.ProgramStatus{}, 0, false)
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
// first report, or records a wait its context ended.
func TestShadowReadiness(t *testing.T) {
	stubShadowFollow(t)
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
		tm.shadowWaitForIdle("s", now, now, nil)
		synctest.Wait()
		if following() != 1 {
			t.Fatal("the first wait started no follow-up")
		}
		for i := 1; i < idleProofsPerHour; i++ {
			tm.shadowWaitForIdle("s", now, time.Now(), nil)
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
	tm.shadowWaitForIdle("s", now, now, nil)
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
	tm.shadowEmit("s", runtime.ProgramPointPending, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone, runtime.ProgramStatus{}, 0, false)
	if got := col.wait(t, 1)[0]; got.Dropped != 1 {
		t.Fatalf("next comparison Dropped = %d, want the panicked one", got.Dropped)
	}
}
