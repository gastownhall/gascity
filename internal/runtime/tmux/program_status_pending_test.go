package tmux

import (
	"fmt"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// approvalScreen is a pane showing a live Claude permission prompt.
const approvalScreen = `● Bash(date)

────────────────────────────────────────────────────────────────────────────────
 Bash command

   date
   Print the date

 Do you want to proceed?
 ❯ 1. Yes
   2. No

 Esc to cancel`

// pendingTestProvider is a tmux provider under mode over ex, its capture
// gate passed and its process table holding readNonce's tap, comparisons
// going to col.
func pendingTestProvider(mode runtime.ProgramStatusMode, ex executor, col *shadowCollector) *Provider {
	table := tapTable(readNonce)
	p, _ := readProvider(mode, "", &table)
	p.tm.exec = ex
	if col != nil {
		p.tm.cfg.ComparisonSink = col.sink
	}
	return p
}

// sinceAgo sets a record's Since d before now.
func sinceAgo(d time.Duration) func(*runtime.ProgramStatus) {
	return func(s *runtime.ProgramStatus) { s.Since = time.Now().Add(-d) }
}

// Kills a pending comparison judged on the state cache's snapshot: the cached
// listing still shows the dialog's blocked record, while the pane's record,
// read fresh with the capture, is back at idle. The record's verdict is
// judged on the fresh reading, and the cache is never read.
func TestShadowPendingJudgesTheFreshRecordNotTheCache(t *testing.T) {
	blocked := psRecord(t, readNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) { s.Kind = runtime.ProgramBlockPermission })
	idle := psRecord(t, readNonce, runtime.ProgramIdle, liveSince, func(s *runtime.ProgramStatus) { s.Seq = 2 })
	ex := &pendingPaneExecutor{fetch: fetchRows(armedPane(blocked)), record: func() string { return idle }}
	col := newShadowCollector()
	p := pendingTestProvider(runtime.ProgramStatusShadow, ex, col)

	if pending, err := p.Pending("s"); pending != nil || err != nil {
		t.Fatalf("Pending = %v, %v; want the parsers' nil, nil", pending, err)
	}
	got := col.wait(t, 1)[0]
	if got.Point != runtime.ProgramPointPending || got.Scrape != runtime.ProgramVerdictNone || got.Program != runtime.ProgramVerdictNone || got.Status.State != runtime.ProgramIdle {
		t.Fatalf("comparison %+v, want pending none/none on the fresh idle reading", got)
	}
	if fetches, fresh := ex.reads(); fetches != 0 || fresh != 1 {
		t.Fatalf("Pending took %d state cache listings and %d fresh reads, want 0 and 1", fetches, fresh)
	}
}

// Kills a pending comparison that lets a pre-close blocked record stand once
// the parsers observed the dialog close: the record entered blocked before
// the dialog, the parsers saw the dialog, the dialog closed and the record
// stayed blocked. Once the parsers see the close, the record's verdict reads
// none; a newer blocked report (entered after the close) stands again.
func TestShadowPendingParserCloseSupersedesPreCloseBlocked(t *testing.T) {
	var record, screen atomic.Value
	record.Store(psRecord(t, readNonce, runtime.ProgramBlocked, sinceAgo(2*time.Second), func(s *runtime.ProgramStatus) { s.Kind = runtime.ProgramBlockPermission }))
	screen.Store(approvalScreen)
	ex := &pendingPaneExecutor{
		record: func() string { return record.Load().(string) },
		screen: func() string { return screen.Load().(string) },
	}
	col := bigShadowCollector(8)
	p := pendingTestProvider(runtime.ProgramStatusShadow, ex, col)

	step := func(n int, wantScrape, wantProgram runtime.ProgramVerdict) {
		t.Helper()
		pending, err := p.Pending("s")
		if err != nil || (pending != nil) != (wantScrape == runtime.ProgramVerdictPending) {
			t.Fatalf("step %d: Pending = %v, %v; want the parsers' %s", n, pending, err, wantScrape)
		}
		got := col.wait(t, n)[n-1]
		if got.Point != runtime.ProgramPointPending || got.Scrape != wantScrape || got.Program != wantProgram || got.Status.State != runtime.ProgramBlocked {
			t.Fatalf("step %d: comparison %+v, want pending %s/%s on the blocked reading", n, got, wantScrape, wantProgram)
		}
	}
	step(1, runtime.ProgramVerdictPending, runtime.ProgramVerdictPending)
	screen.Store("❯ ")
	step(2, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone)
	step(3, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone)

	// The newer report enters blocked after the close the parsers observed,
	// on the record's millisecond clock.
	closed := p.tm.dialogWatch("s").ClosedAt
	for !time.Now().After(closed.Add(time.Millisecond)) {
		goruntime.Gosched()
	}
	record.Store(psRecord(t, readNonce, runtime.ProgramBlocked, liveSince, func(s *runtime.ProgramStatus) {
		s.Kind, s.Seq = runtime.ProgramBlockQuestion, 5
	}))
	step(4, runtime.ProgramVerdictNone, runtime.ProgramVerdictPending)
}

// Kills a pending read that, with the switch off, reads the record or keeps
// a dialog watch: off and on (whose decisions do not use the record yet)
// answer exactly as the parsers do and take no program read of any kind.
func TestPendingReadsNoRecordUnlessShadowing(t *testing.T) {
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusOn} {
		t.Run(string(mode), func(t *testing.T) {
			blocked := psRecord(t, readNonce, runtime.ProgramBlocked, liveSince)
			var screen atomic.Value
			screen.Store(approvalScreen)
			ex := &pendingPaneExecutor{
				fetch:  fetchRows(armedPane(blocked)),
				record: func() string { return blocked },
				screen: func() string { return screen.Load().(string) },
			}
			p := pendingTestProvider(mode, ex, newShadowCollector())
			if pending, err := p.Pending("s"); pending == nil || err != nil {
				t.Fatalf("Pending = %v, %v; want the parsers' approval", pending, err)
			}
			screen.Store("❯ ")
			if pending, err := p.Pending("s"); pending != nil || err != nil {
				t.Fatalf("Pending = %v, %v; want the parsers' nil, nil", pending, err)
			}
			if fetches, fresh := ex.reads(); fetches != 0 || fresh != 0 {
				t.Fatalf("Pending took %d state cache listings and %d fresh reads, want none", fetches, fresh)
			}
			if w := p.tm.dialogWatch("s"); w != (runtime.ProgramDialogWatch{}) {
				t.Fatalf("dialog watch %+v, want none kept", w)
			}
		})
	}
}

// Kills a pendingWithRecord that reads or keeps anything with the switch
// off, or that, with the switch on (the cutover's reuse), does not pair the
// capture with a fresh read and the dialog watch.
func TestPendingWithRecordPairsAFreshRead(t *testing.T) {
	blocked := psRecord(t, readNonce, runtime.ProgramBlocked, sinceAgo(time.Second))
	var screen atomic.Value
	screen.Store(approvalScreen)
	ex := &pendingPaneExecutor{record: func() string { return blocked }, screen: func() string { return screen.Load().(string) }}

	off := pendingTestProvider(runtime.ProgramStatusOff, ex, nil)
	if pending, r, ok, err := off.pendingWithRecord("s"); pending == nil || ok || err != nil || r != (runtime.ProgramPendingReading{}) {
		t.Fatalf("off: pendingWithRecord = %v, %+v, %t, %v; want the parsers' approval and no reading", pending, r, ok, err)
	}
	if _, fresh := ex.reads(); fresh != 0 {
		t.Fatalf("off took %d fresh reads, want none", fresh)
	}

	on := pendingTestProvider(runtime.ProgramStatusOn, ex, nil)
	pending, r, ok, err := on.pendingWithRecord("s")
	if pending == nil || !ok || err != nil || !r.ParserPending || !r.Status.Blocked() || !r.Status.Decides() {
		t.Fatalf("on, dialog: pendingWithRecord = %v, %+v, %t, %v; want the approval paired with a deciding blocked reading", pending, r, ok, err)
	}
	if runtime.ProgramFreshPendingVerdict(r, time.Time{}, time.Now()) != runtime.ProgramVerdictPending {
		t.Fatalf("on, dialog: fresh verdict on %+v is not pending", r)
	}
	screen.Store("❯ ")
	pending, r, ok, err = on.pendingWithRecord("s")
	if pending != nil || !ok || err != nil || r.ParserPending || !r.CloseSupersedes() {
		t.Fatalf("on, closed: pendingWithRecord = %v, %+v, %t, %v; want the close to supersede the blocked reading", pending, r, ok, err)
	}
	if _, fresh := ex.reads(); fresh != 2 {
		t.Fatalf("on took %d fresh reads, want 2", fresh)
	}
}

// Kills a typing gate comparison that lets a pre-close blocked record stand
// once the gate's own looks observed the dialog close, or one that takes a
// look that could not read the pane for a close.
func TestShadowTypingGateParserCloseSupersedesPreCloseBlocked(t *testing.T) {
	col := bigShadowCollector(8)
	tm, _ := shadowTmux(runtime.ProgramStatusShadow, &fakeExecutor{}, nil, col.sink)
	var blocked runtime.ProgramStatus
	blocked.Mode, blocked.Validity, blocked.State, blocked.Nonce = runtime.ProgramStatusShadow, runtime.ProgramStatusValid, runtime.ProgramBlocked, readNonce
	blocked.Since = time.Now().Add(-2 * time.Second)
	refusal := fmt.Errorf("%w: a dialog", runtime.ErrPendingInteraction)

	gate := func(n int, lookErr error, want runtime.ProgramVerdict) {
		t.Helper()
		tm.shadowTypingGate("s", lookErr, time.Now(), blocked)
		got := col.wait(t, n)[n-1]
		if got.Point != runtime.ProgramPointTypingGate || got.Program != want {
			t.Fatalf("look %d: comparison %+v, want program %s", n, got, want)
		}
	}
	gate(1, nil, runtime.ProgramVerdictPending) // no dialog seen yet: the record stands
	gate(2, refusal, runtime.ProgramVerdictPending)
	gate(3, fmt.Errorf("capture failed"), runtime.ProgramVerdictPending) // no observation
	gate(4, nil, runtime.ProgramVerdictNone)                             // the close the parsers saw
}
