package ledgerfeed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/journalops"
)

// fakeJournal is a scripted journalops.Journal: an in-memory row log with a
// head counter and a retention floor, behaving as the role documents.
type fakeJournal struct {
	mu    sync.Mutex
	rows  []journalops.Row // seq-ascending
	head  int64
	floor int64 // lowest retained seq; 0 or 1 retains everything
	fail  error // returned once by the next read, then cleared
	reads []readCall
}

type readCall struct {
	since int64
	limit int
}

func (f *fakeJournal) append(issueIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range issueIDs {
		f.head++
		f.rows = append(f.rows, journalops.Row{
			Seq: f.head, TS: fmt.Sprintf("ts-%d-%s", f.head, id), Op: "update", IssueID: id,
			IssueJSON: `{"id":"` + id + `"}`,
		})
	}
}

// prune drops rows below floor, leaving the head untouched.
func (f *fakeJournal) prune(floor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.floor = floor
	f.rows = slices.DeleteFunc(f.rows, func(r journalops.Row) bool { return r.Seq < floor })
}

// reseed wipes the journal and restarts its counter at seq, as a recreated
// journal table seeded at the old high-water mark would.
func (f *fakeJournal) reseed(seq int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = nil
	f.head = seq
	f.floor = 0
}

func (f *fakeJournal) ReadEventsJournalPage(_ context.Context, since int64, limit int) (journalops.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, readCall{since: since, limit: limit})
	if err := f.fail; err != nil {
		f.fail = nil
		return journalops.Page{}, err
	}
	if since >= f.head {
		return journalops.Page{Head: f.head}, nil
	}
	if f.floor > 1 && since < f.floor-1 {
		return journalops.Page{}, &journalops.TruncatedError{Since: since, Floor: f.floor, Head: f.head}
	}
	var out []journalops.Row
	for _, r := range f.rows {
		if r.Seq > since && (limit == 0 || len(out) < limit) {
			out = append(out, r)
		}
	}
	return journalops.Page{Rows: out, Head: f.head}, nil
}

func (f *fakeJournal) readLog() []readCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reads)
}

// recorder collects applied rows and gaps.
type recorder struct {
	mu      sync.Mutex
	applied []int64
	gaps    []Gap
	failN   int // fail the next N Apply calls
}

func (r *recorder) apply(_ context.Context, page journalops.Page) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failN > 0 {
		r.failN--
		return errors.New("apply refused")
	}
	for _, row := range page.Rows {
		r.applied = append(r.applied, row.Seq)
	}
	return nil
}

func (r *recorder) onGap(g Gap) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gaps = append(r.gaps, g)
}

func (r *recorder) seqs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.applied)
}

func newTailer(j *fakeJournal, r *recorder) *Tailer {
	return &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap, PageLimit: 2}
}

func mustStep(t *testing.T, tl *Tailer) bool {
	t.Helper()
	more, err := tl.Step(context.Background())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	return more
}

func TestStepAppliesInOrderAndReportsMoreAfterAFullPage(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	r := &recorder{}
	tl := newTailer(j, r)

	if !mustStep(t, tl) {
		t.Fatal("a full page must ask for an immediate re-read")
	}
	if got := tl.Checkpoint(); got.Seq != 2 || got.Last.IssueID != "b" || got.Last.Seq != 2 {
		t.Fatalf("checkpoint after first page = %+v, want seq 2 fingerprinted on b", got)
	}
	if mustStep(t, tl) {
		t.Fatal("a short page at the head must not ask for a re-read")
	}
	if mustStep(t, tl) {
		t.Fatal("an idle read must not ask for a re-read")
	}
	if got, want := r.seqs(), []int64{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	stats := tl.Stats()
	if stats.RowsApplied != 3 || stats.Pages != 2 || stats.Seq != 3 || stats.Head != 3 || stats.Lag != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	for _, call := range j.readLog() {
		if call.limit != 2 {
			t.Fatalf("read with limit %d, want PageLimit 2", call.limit)
		}
	}
}

func TestStepReportsLagWhileBehind(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c", "d", "e")
	r := &recorder{}
	tl := newTailer(j, r)
	mustStep(t, tl)
	if stats := tl.Stats(); stats.Lag != 3 || stats.Head != 5 {
		t.Fatalf("stats = %+v, want lag 3 behind head 5", stats)
	}
}

func TestApplyErrorRetriesTheSamePage(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	r := &recorder{failN: 1}
	tl := newTailer(j, r)

	more, err := tl.Step(context.Background())
	if err == nil || more {
		t.Fatalf("Step = (%v, %v), want an apply error and no re-read", more, err)
	}
	if got := tl.Checkpoint(); got.Seq != 0 {
		t.Fatalf("checkpoint advanced to %d on an Apply error", got.Seq)
	}
	mustStep(t, tl)
	if got, want := r.seqs(), []int64{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	if reads := j.readLog(); reads[0].since != 0 || reads[1].since != 0 {
		t.Fatalf("reads = %+v, want the same checkpoint re-read", reads)
	}
	if stats := tl.Stats(); stats.Errors != 1 {
		t.Fatalf("Errors = %d, want 1", stats.Errors)
	}
}

func TestReadErrorLeavesCheckpointAndIsReturned(t *testing.T) {
	j := &fakeJournal{}
	j.append("a")
	boom := errors.New("connection reset")
	j.fail = boom
	tl := newTailer(j, &recorder{})
	if _, err := tl.Step(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Step error = %v, want it to wrap %v", err, boom)
	}
	if got := tl.Checkpoint(); got.Seq != 0 {
		t.Fatalf("checkpoint = %+v after a failed read", got)
	}
}

func TestTruncationGapRebaselinesAtHeadAndNeverReplaysFromZero(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	r := &recorder{}
	tl := newTailer(j, r)
	mustStep(t, tl) // applied 1..2
	j.append("c", "d", "e", "f")
	j.prune(5) // rows 3 and 4 are gone before they were read

	if !mustStep(t, tl) {
		t.Fatal("a gap must ask for an immediate re-read from the new baseline")
	}
	if len(r.gaps) != 1 {
		t.Fatalf("gaps = %+v, want one", r.gaps)
	}
	gap := r.gaps[0]
	if gap.Kind != GapTruncated || gap.From.Seq != 2 || gap.Floor != 5 || gap.Head != 6 || gap.To.Seq != 6 || gap.To.Last.IssueID != "f" {
		t.Fatalf("gap = %+v", gap)
	}
	var truncated *journalops.TruncatedError
	if !errors.As(gap.Err, &truncated) {
		t.Fatalf("gap.Err = %v, want the *journalops.TruncatedError", gap.Err)
	}
	j.append("g")
	mustStep(t, tl)
	if got, want := r.seqs(), []int64{1, 2, 7}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v (rows 3..6 are the consumer's resync, not a replay)", got, want)
	}
	for i, call := range j.readLog() {
		if i > 0 && call.since == 0 {
			t.Fatalf("read %d replayed from seq 0: %+v", i, j.readLog())
		}
	}
	if stats := tl.Stats(); stats.Gaps[GapTruncated] != 1 {
		t.Fatalf("stats gaps = %+v", stats.Gaps)
	}
}

func TestTruncatedBaselineRowGetsAnUnverifiableCheckpoint(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	j.prune(4) // everything pruned; head stays 3
	r := &recorder{}
	tl := newTailer(j, r)
	tl.Resume(Checkpoint{Seq: 1}) // unverifiable: no fingerprint

	mustStep(t, tl)
	if len(r.gaps) != 1 || r.gaps[0].Kind != GapTruncated {
		t.Fatalf("gaps = %+v, want one truncation", r.gaps)
	}
	if got := tl.Checkpoint(); got.Seq != 3 || got.Last.Seq != 0 {
		t.Fatalf("checkpoint = %+v, want seq 3 with no fingerprint (row pruned)", got)
	}
}

func TestHeadRegressedGap(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	r := &recorder{}
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap}
	mustStep(t, tl)
	j.reseed(0)
	j.append("x")

	if !mustStep(t, tl) {
		t.Fatal("a gap must ask for an immediate re-read")
	}
	if len(r.gaps) != 1 || r.gaps[0].Kind != GapHeadRegressed || r.gaps[0].Head != 1 || r.gaps[0].From.Seq != 3 {
		t.Fatalf("gaps = %+v, want one head regression from 3 to head 1", r.gaps)
	}
	if got := tl.Checkpoint(); got.Seq != 1 || got.Last.IssueID != "x" {
		t.Fatalf("checkpoint = %+v, want re-baselined at head 1 on x", got)
	}
	j.append("y")
	mustStep(t, tl)
	if got, want := r.seqs(), []int64{1, 2, 3, 2}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
}

func TestResumeDetectsACounterReseededAtTheHighWaterMark(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	r := &recorder{}
	first := &Tailer{Source: j, Apply: r.apply}
	mustStep(t, first)
	saved := first.Checkpoint()

	// While the consumer was away the journal was recreated and its counter
	// seeded at the old head; new writes landed above it. The head did not
	// regress, so only the fingerprint can tell.
	j.reseed(2)
	j.append("p", "q")

	r2 := &recorder{}
	tl := &Tailer{Source: j, Apply: r2.apply, OnGap: r2.onGap}
	tl.Resume(saved)
	if !mustStep(t, tl) {
		t.Fatal("a gap must ask for an immediate re-read")
	}
	if len(r2.gaps) != 1 || r2.gaps[0].Kind != GapReset || r2.gaps[0].From != saved {
		t.Fatalf("gaps = %+v, want one reset from %+v", r2.gaps, saved)
	}
	if got := tl.Checkpoint(); got.Seq != 4 || got.Last.IssueID != "q" {
		t.Fatalf("checkpoint = %+v, want re-baselined at head 4", got)
	}
	if len(r2.seqs()) != 0 {
		t.Fatalf("applied %v across a reset; the consumer must resync instead", r2.seqs())
	}
}

func TestResumeWithAMatchingFingerprintContinues(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	r := &recorder{}
	first := &Tailer{Source: j, Apply: r.apply}
	mustStep(t, first)
	j.append("c")

	r2 := &recorder{}
	tl := &Tailer{Source: j, Apply: r2.apply, OnGap: r2.onGap}
	tl.Resume(first.Checkpoint())
	mustStep(t, tl)
	if len(r2.gaps) != 0 {
		t.Fatalf("gaps = %+v on a healthy resume", r2.gaps)
	}
	if got, want := r2.seqs(), []int64{3}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	reads := j.readLog()
	verify := reads[len(reads)-2]
	if verify.since != 1 || verify.limit != 1 {
		t.Fatalf("verification read = %+v, want since=seq-1=1, limit=1", verify)
	}
}

func TestIdlePollsReverifyTheFingerprint(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	r := &recorder{}
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap, VerifyEvery: 3}
	mustStep(t, tl)
	// Same head, different row: a reset only the fingerprint sees.
	j.reseed(1)
	j.append("z")
	for range 3 {
		mustStep(t, tl) // idle reads
	}
	if len(r.gaps) != 0 {
		t.Fatalf("gap reported before the verification was due: %+v", r.gaps)
	}
	mustStep(t, tl) // verification
	if len(r.gaps) != 1 || r.gaps[0].Kind != GapReset {
		t.Fatalf("gaps = %+v, want one reset found by the idle verification", r.gaps)
	}
}

func TestBaselineStartsAtTheHeadWithAFingerprint(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	r := &recorder{}
	tl := &Tailer{Source: j, Apply: r.apply}
	cp, err := tl.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if cp.Seq != 3 || cp.Last != (Fingerprint{Seq: 3, IssueID: "c", TS: "ts-3-c"}) {
		t.Fatalf("Baseline = %+v", cp)
	}
	j.append("d")
	mustStep(t, tl)
	if got, want := r.seqs(), []int64{4}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
}

func TestBaselineOnAnEmptyJournal(t *testing.T) {
	j := &fakeJournal{}
	tl := &Tailer{Source: j, Apply: (&recorder{}).apply}
	cp, err := tl.Baseline(context.Background())
	if err != nil || cp != (Checkpoint{}) {
		t.Fatalf("Baseline = (%+v, %v), want the zero checkpoint", cp, err)
	}
}

func TestStepRefusesAnUnconfiguredTailer(t *testing.T) {
	if _, err := (&Tailer{Apply: (&recorder{}).apply}).Step(context.Background()); err == nil {
		t.Fatal("Step without a Source succeeded")
	}
	if _, err := (&Tailer{Source: &fakeJournal{}}).Step(context.Background()); err == nil {
		t.Fatal("Step without Apply succeeded")
	}
}

// scriptedSleep records requested delays and cancels ctx after a number of
// sleeps, standing in for the clock.
type scriptedSleep struct {
	delays []time.Duration
	after  int
	cancel context.CancelFunc
	hook   func(n int)
}

func (s *scriptedSleep) sleep(ctx context.Context, d time.Duration) error {
	s.delays = append(s.delays, d)
	if s.hook != nil {
		s.hook(len(s.delays))
	}
	if len(s.delays) >= s.after {
		s.cancel()
	}
	return ctx.Err()
}

func TestRunBacksOffWhenIdleAndReadsAtOnceAfterAFullPage(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c", "d", "e")
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	clock := &scriptedSleep{after: 5, cancel: cancel}
	clock.hook = func(n int) {
		if n == 3 {
			j.append("f")
		}
	}
	tl := &Tailer{Source: j, Apply: r.apply, PageLimit: 2, Sleep: clock.sleep}

	if err := tl.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	// Pages 1-2, 3-4 are full and re-read at once; 5 ends at the head (sleep
	// 250ms); idle (500ms); idle (1s); row 6 arrives and is applied, which
	// resets the backoff (250ms); idle (500ms).
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 250 * time.Millisecond, 500 * time.Millisecond}
	if !slices.Equal(clock.delays, want) {
		t.Fatalf("delays = %v, want %v", clock.delays, want)
	}
	if got, want := r.seqs(), []int64{1, 2, 3, 4, 5, 6}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
}

func TestRunCapsIdleBackoff(t *testing.T) {
	j := &fakeJournal{}
	ctx, cancel := context.WithCancel(context.Background())
	clock := &scriptedSleep{after: 5, cancel: cancel}
	tl := &Tailer{Source: j, Apply: (&recorder{}).apply, Sleep: clock.sleep}
	_ = tl.Run(ctx)
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, time.Second, time.Second}
	if !slices.Equal(clock.delays, want) {
		t.Fatalf("delays = %v, want %v", clock.delays, want)
	}
}

func TestRunReportsErrorsAndRetries(t *testing.T) {
	j := &fakeJournal{}
	j.append("a")
	r := &recorder{failN: 1}
	ctx, cancel := context.WithCancel(context.Background())
	clock := &scriptedSleep{after: 2, cancel: cancel}
	var reported []error
	tl := &Tailer{Source: j, Apply: r.apply, Sleep: clock.sleep, OnError: func(err error) { reported = append(reported, err) }}
	_ = tl.Run(ctx)
	if len(reported) != 1 {
		t.Fatalf("OnError saw %v, want one apply failure", reported)
	}
	if got, want := r.seqs(), []int64{1}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v after the retry", got, want)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	j := &fakeJournal{}
	ctx, cancel := context.WithCancel(context.Background())
	tl := &Tailer{Source: j, Apply: (&recorder{}).apply, Poll: time.Millisecond, IdlePoll: time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- tl.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
