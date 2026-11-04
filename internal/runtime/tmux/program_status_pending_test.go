package tmux

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
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

// storeRecord is the record store holds, encoded as the capture publishes
// it on the pane.
func storeRecord(t *testing.T, store *programstatus.Store) string {
	t.Helper()
	v, err := programstatus.Encode(store.Status())
	if err != nil {
		t.Fatalf("encoding the store's record: %v", err)
	}
	return v
}

// blockedOn applies a root blocked report of kind to store at at.
func blockedOn(store *programstatus.Store, kind runtime.ProgramBlockKind, at time.Time) {
	store.Apply(programstatus.Report{State: runtime.ProgramBlocked, Kind: kind, Progress: -1}, at)
}

// pendingStep is the n-th Pending read of p's session "s": it fails unless
// the parsers answered wantScrape and the pending comparison col received
// for it judged wantScrape and wantProgram on a blocked reading.
func pendingStep(t *testing.T, p *Provider, col *shadowCollector, n int, wantScrape, wantProgram runtime.ProgramVerdict) {
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

// Kills a pending comparison that lets a pre-close blocked record stand
// once the parsers observed the dialog close, or that supersedes the newer
// report after it (review finding: a close judged against Since): the
// program reported blocked, the parsers saw the dialog, the dialog closed
// and the record stayed blocked, so once the parsers see the close the
// record's verdict reads none. Then the program reports blocked for another
// dialog the parsers cannot see: the store keeps the root's Since (the root
// never left blocked) and moves Seq, and that record stands.
func TestShadowPendingParserCloseSupersedesPreCloseBlocked(t *testing.T) {
	store := programstatus.NewStore(readNonce)
	blockedOn(store, runtime.ProgramBlockPermission, time.Now().Add(-2*time.Second))
	var record, screen atomic.Value
	record.Store(storeRecord(t, store))
	screen.Store(approvalScreen)
	ex := &pendingPaneExecutor{
		record: func() string { return record.Load().(string) },
		screen: func() string { return screen.Load().(string) },
	}
	col := bigShadowCollector(8)
	p := pendingTestProvider(runtime.ProgramStatusShadow, ex, col)

	pendingStep(t, p, col, 1, runtime.ProgramVerdictPending, runtime.ProgramVerdictPending)
	screen.Store("❯ ")
	pendingStep(t, p, col, 2, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone)
	pendingStep(t, p, col, 3, runtime.ProgramVerdictNone, runtime.ProgramVerdictNone)

	seen := store.Status()
	blockedOn(store, runtime.ProgramBlockAuth, time.Now())
	if now := store.Status(); !now.Since.Equal(seen.Since) || now.Seq == seen.Seq {
		t.Fatalf("blocked for another dialog: record %+v after %+v, want Since kept and Seq moved", now, seen)
	}
	record.Store(storeRecord(t, store))
	pendingStep(t, p, col, 4, runtime.ProgramVerdictNone, runtime.ProgramVerdictPending)
}

// Kills a pending comparison that pins a close the parsers observed on a
// record that changed before the capture observing it (review finding: the
// close is stamped at that capture, so a record that entered blocked again
// in between read as pre-close): the parsers saw the dialog with one record,
// and before their next capture showed it gone the program left blocked and
// entered it again for a dialog they cannot see, or the pane respawned under
// a new incarnation whose program reports blocked (the same Seq, a new
// Nonce). Neither is the record the dialog was seen with, so each stands.
func TestShadowPendingRecordChangedBeforeTheObservedCloseStands(t *testing.T) {
	cases := []struct {
		name  string
		nonce string // the incarnation the dialog is seen under
		// change returns the store the pane publishes once the record
		// changed, and the nonce the pane is then armed under.
		change func(t *testing.T, seen *programstatus.Store) (*programstatus.Store, string)
	}{
		{"blocked again for another dialog", readNonce, func(_ *testing.T, seen *programstatus.Store) (*programstatus.Store, string) {
			seen.Apply(programstatus.Report{State: runtime.ProgramWorking, Progress: -1}, time.Now())
			blockedOn(seen, runtime.ProgramBlockAuth, time.Now())
			return seen, readNonce
		}},
		{"respawned under a new incarnation", readOldNonce, func(t *testing.T, seen *programstatus.Store) (*programstatus.Store, string) {
			respawned := programstatus.NewStore(readNonce)
			blockedOn(respawned, runtime.ProgramBlockAuth, time.Now())
			if got, want := respawned.Status().Seq, seen.Status().Seq; got != want {
				t.Fatalf("respawned record's Seq %d, want the seen record's %d: only the Nonce may tell them apart", got, want)
			}
			return respawned, readNonce
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen := programstatus.NewStore(c.nonce)
			blockedOn(seen, runtime.ProgramBlockPermission, time.Now().Add(-2*time.Second))
			var record, tap, screen atomic.Value
			record.Store(storeRecord(t, seen))
			tap.Store(c.nonce)
			screen.Store(approvalScreen)
			ex := &pendingPaneExecutor{
				record: func() string { return record.Load().(string) },
				tap:    func() string { return tap.Load().(string) },
				screen: func() string { return screen.Load().(string) },
			}
			col := bigShadowCollector(4)
			p := pendingTestProvider(runtime.ProgramStatusShadow, ex, col)
			pendingStep(t, p, col, 1, runtime.ProgramVerdictPending, runtime.ProgramVerdictPending)

			store, nonce := c.change(t, seen)
			record.Store(storeRecord(t, store))
			tap.Store(nonce)
			screen.Store("❯ ")
			pendingStep(t, p, col, 2, runtime.ProgramVerdictNone, runtime.ProgramVerdictPending)
		})
	}
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
// once the gate's own looks observed the dialog close, one that takes a look
// that could not read the pane for a close, or one that supersedes on such a
// look while the latest look that read the pane showed the dialog again
// (review finding: a failed look judged as one showing no dialog).
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
	gate(5, refusal, runtime.ProgramVerdictPending)                      // the dialog again
	gate(6, fmt.Errorf("capture failed"), runtime.ProgramVerdictPending) // the latest look showed it
	gate(7, nil, runtime.ProgramVerdictNone)                             // its close
}
