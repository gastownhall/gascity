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
// head counter. Its truncation follows beads' ComputeEventsTruncation: a read
// whose first row is not since+1, or that finds nothing while since is below
// the head, is a *journalops.TruncatedError.
type fakeJournal struct {
	mu      sync.Mutex
	rows    []journalops.Row // seq-ascending
	head    int64
	failErr error // returned once by the next read, then cleared
	reads   []readCall
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

// appendRaw appends rows as given, numbered on from the head, for a row
// append cannot build.
func (f *fakeJournal) appendRaw(rows ...journalops.Row) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, row := range rows {
		f.head++
		row.Seq = f.head
		f.rows = append(f.rows, row)
	}
}

// prune drops rows below floor, leaving the head untouched.
func (f *fakeJournal) prune(floor int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = slices.DeleteFunc(f.rows, func(r journalops.Row) bool { return r.Seq < floor })
}

// reseed wipes the journal and restarts its counter at seq, as a journal
// table recreated with its counter seeded at seq would.
func (f *fakeJournal) reseed(seq int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = nil
	f.head = seq
}

func (f *fakeJournal) ReadEventsJournalPage(_ context.Context, since int64, limit int) (journalops.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, readCall{since: since, limit: limit})
	if err := f.failErr; err != nil {
		f.failErr = nil
		return journalops.Page{}, err
	}
	var out []journalops.Row
	for _, r := range f.rows {
		if r.Seq > since && (limit == 0 || len(out) < limit) {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		if since >= f.head {
			return journalops.Page{Head: f.head}, nil
		}
		return journalops.Page{}, &journalops.TruncatedError{Since: since, Floor: f.head + 1, Head: f.head}
	}
	if out[0].Seq != since+1 {
		return journalops.Page{}, &journalops.TruncatedError{Since: since, Floor: out[0].Seq, Head: f.head}
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
	mu       sync.Mutex
	applied  []int64
	gaps     []Gap
	failN    int // fail the next N Apply calls
	gapFailN int // fail the next N OnGap calls
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

func (r *recorder) onGap(_ context.Context, g Gap) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gapFailN > 0 {
		r.gapFailN--
		return errors.New("resync failed")
	}
	r.gaps = append(r.gaps, g)
	return nil
}

func (r *recorder) seqs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.applied)
}

func (r *recorder) gapList() []Gap {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.gaps)
}

func newTailer(j journalops.Journal, r *recorder) *Tailer {
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
	// The first read has no row to check; every later one re-reads the
	// checkpoint row and asks for one extra.
	want := []readCall{{since: 0, limit: 2}, {since: 1, limit: 3}, {since: 2, limit: 3}}
	if got := j.readLog(); !slices.Equal(got, want) {
		t.Fatalf("reads = %+v, want %+v", got, want)
	}
}

func TestStepReportsLagWhileBehind(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c", "d", "e")
	tl := newTailer(j, &recorder{})
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
	j.failErr = boom
	tl := newTailer(j, &recorder{})
	more, err := tl.Step(context.Background())
	if !errors.Is(err, boom) || more {
		t.Fatalf("Step = (%v, %v), want it to wrap %v with no re-read", more, err, boom)
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
	gaps := r.gapList()
	if len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want one", gaps)
	}
	gap := gaps[0]
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
	mustResume(t, tl, Checkpoint{Seq: 1}) // unverifiable: no fingerprint

	mustStep(t, tl)
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].Kind != GapTruncated {
		t.Fatalf("gaps = %+v, want one truncation", gaps)
	}
	if got := tl.Checkpoint(); got.Seq != 3 || got.Last.Seq != 0 {
		t.Fatalf("checkpoint = %+v, want seq 3 with no fingerprint (row pruned)", got)
	}
}

func TestOnGapErrorKeepsTheCheckpointAndRetries(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	r := &recorder{gapFailN: 1}
	tl := newTailer(j, r)
	mustStep(t, tl)
	saved := tl.Checkpoint()
	j.append("c", "d", "e")
	j.prune(5)

	more, err := tl.Step(context.Background())
	if err == nil || more {
		t.Fatalf("Step = (%v, %v), want OnGap's error and no re-read", more, err)
	}
	if got := tl.Checkpoint(); got != saved {
		t.Fatalf("checkpoint moved to %+v though the resync failed", got)
	}
	if stats := tl.Stats(); len(stats.Gaps) != 0 || stats.Errors != 1 {
		t.Fatalf("stats = %+v, want no gap counted and one error", stats)
	}
	mustStep(t, tl)
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].From != saved {
		t.Fatalf("gaps = %+v, want the same gap offered again", gaps)
	}
	if got := tl.Checkpoint(); got.Seq != 5 {
		t.Fatalf("checkpoint = %+v, want the head after the resync succeeded", got)
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
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].Kind != GapHeadRegressed || gaps[0].Head != 1 || gaps[0].From.Seq != 3 {
		t.Fatalf("gaps = %+v, want one head regression from 3 to head 1", gaps)
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

func TestResumeDetectsACounterReseededBelowTheHighWaterMark(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	first := newTailer(j, &recorder{})
	mustStep(t, first)
	mustStep(t, first)
	saved := first.Checkpoint()

	// While the consumer was away the journal was recreated with its counter
	// seeded below the old head, and new writes passed it. The head did not
	// regress, so only the fingerprint can tell.
	j.reseed(2)
	j.append("p", "q")

	r := &recorder{}
	tl := newTailer(j, r)
	mustResume(t, tl, saved)
	if !mustStep(t, tl) {
		t.Fatal("a gap must ask for an immediate re-read")
	}
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].Kind != GapReset || gaps[0].From != saved {
		t.Fatalf("gaps = %+v, want one reset from %+v", gaps, saved)
	}
	if got := tl.Checkpoint(); got.Seq != 4 || got.Last.IssueID != "q" {
		t.Fatalf("checkpoint = %+v, want re-baselined at head 4", got)
	}
	if len(r.seqs()) != 0 {
		t.Fatalf("applied %v across a reset; the consumer must resync instead", r.seqs())
	}
}

// A counter re-seeded exactly at the checkpoint: the checkpoint row is gone
// and every new row is above it, which a conforming journal reports as a
// truncation of the verification read.
func TestReseedAtTheCheckpointIsAReset(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	first := newTailer(j, &recorder{})
	mustStep(t, first)
	mustStep(t, first)
	saved := first.Checkpoint() // {3, c}

	j.reseed(3)
	j.append("p", "q")

	r := &recorder{}
	tl := newTailer(j, r)
	mustResume(t, tl, saved)
	mustStep(t, tl)
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].Kind != GapReset {
		t.Fatalf("gaps = %+v, want one reset", gaps)
	}
	if len(r.seqs()) != 0 {
		t.Fatalf("applied %v across a reset", r.seqs())
	}
}

// A busy feed: the counter resets and new rows overtake the checkpoint
// between two reads. No resume, no idle poll — the next read must catch it.
func TestABusyFeedDetectsAResetOnTheNextRead(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c", "d", "e")
	r := &recorder{}
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap}
	mustStep(t, tl) // checkpoint {5, e}

	j.reseed(2)
	j.append("v", "w", "x", "y", "z") // seqs 3..7

	mustStep(t, tl)
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].Kind != GapReset || gaps[0].To.Seq != 7 {
		t.Fatalf("gaps = %+v, want one reset re-baselined at 7", gaps)
	}
	if got, want := r.seqs(), []int64{1, 2, 3, 4, 5}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v (no new-history row applied as a continuation)", got, want)
	}
}

// A re-seeded counter can write the checkpoint's issue at the checkpoint's
// seq with the same TS, which is a label rather than an identity. The rest of
// the row still tells it from the original.
func TestAReseedTheTimestampCannotTellIsStillAReset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*journalops.Row)
		reset  bool
	}{
		{name: "the same row", mutate: func(*journalops.Row) {}},
		{name: "another op", mutate: func(r *journalops.Row) { r.Op = "close" }, reset: true},
		{name: "another post-image", mutate: func(r *journalops.Row) { r.IssueJSON = `{"id":"c","status":"closed"}` }, reset: true},
		{name: "another actor", mutate: func(r *journalops.Row) { r.Actor = "someone-else" }, reset: true},
		{name: "a payload byte moved to the next field", mutate: func(r *journalops.Row) {
			r.IssueJSON, r.DepJSON = `{"id":"c"`, `}`
		}, reset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &fakeJournal{}
			j.append("a", "b", "c")
			first := newTailer(j, &recorder{})
			mustStep(t, first)
			mustStep(t, first)
			saved := first.Checkpoint() // {3, c}

			reseeded := journalops.Row{TS: "ts-3-c", Op: "update", IssueID: "c", IssueJSON: `{"id":"c"}`}
			tc.mutate(&reseeded)
			j.reseed(2)
			j.appendRaw(reseeded, journalops.Row{TS: "ts-4-d", Op: "update", IssueID: "d"})

			r := &recorder{}
			tl := newTailer(j, r)
			mustResume(t, tl, saved)
			mustStep(t, tl)
			gaps := r.gapList()
			if !tc.reset {
				if got, want := r.seqs(), []int64{4}; len(gaps) != 0 || !slices.Equal(got, want) {
					t.Fatalf("gaps = %+v, applied %v; want no gap and %v: the row is the original", gaps, got, want)
				}
				return
			}
			if len(gaps) != 1 || gaps[0].Kind != GapReset {
				t.Fatalf("gaps = %+v, want one reset", gaps)
			}
			if len(r.seqs()) != 0 {
				t.Fatalf("applied %v across a reset", r.seqs())
			}
		})
	}
}

// Apply may reuse the rows it is handed; the checkpoint's fingerprint is the
// journal's row, not what Apply left of it.
func TestAnApplyThatMutatesItsRowsDoesNotFakeAReset(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	r := &recorder{}
	tl := newTailer(j, r)
	tl.Apply = func(ctx context.Context, page journalops.Page) error {
		err := r.apply(ctx, page)
		for i := range page.Rows {
			page.Rows[i] = journalops.Row{}
		}
		return err
	}
	mustStep(t, tl) // applies 1..2
	mustStep(t, tl) // re-reads row 2 and applies 3
	if gaps := r.gapList(); len(gaps) != 0 {
		t.Fatalf("gaps = %+v: the row Apply rewrote was taken for the journal's", gaps)
	}
	if got, want := r.seqs(), []int64{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
}

func TestResumeWithAMatchingFingerprintContinues(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	first := &Tailer{Source: j, Apply: (&recorder{}).apply, OnGap: (&recorder{}).onGap}
	mustStep(t, first)
	j.append("c")

	r := &recorder{}
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap}
	mustResume(t, tl, first.Checkpoint())
	mustStep(t, tl)
	if gaps := r.gapList(); len(gaps) != 0 {
		t.Fatalf("gaps = %+v on a healthy resume", gaps)
	}
	if got, want := r.seqs(), []int64{3}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	reads := j.readLog()
	if last := reads[len(reads)-1]; last.since != 1 || last.limit != DefaultPageLimit+1 {
		t.Fatalf("resume read = %+v, want since=seq-1=1 with one extra row", last)
	}
}

// skippingJournal serves rows above since+1 without reporting truncation,
// which a conforming journal never does.
type skippingJournal struct{ rows []journalops.Row }

func (s *skippingJournal) ReadEventsJournalPage(_ context.Context, since int64, _ int) (journalops.Page, error) {
	var out []journalops.Row
	for _, r := range s.rows {
		if r.Seq > since {
			out = append(out, r)
		}
	}
	return journalops.Page{Rows: out, Head: s.rows[len(s.rows)-1].Seq}, nil
}

func TestANonConformingSourceCannotMakeTheTailerSkipRows(t *testing.T) {
	src := &skippingJournal{rows: []journalops.Row{{Seq: 3, IssueID: "c"}, {Seq: 4, IssueID: "d"}}}
	r := &recorder{}
	tl := &Tailer{Source: src, Apply: r.apply, OnGap: r.onGap}
	mustStep(t, tl)
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].Kind != GapTruncated || gaps[0].Floor != 3 {
		t.Fatalf("gaps = %+v, want a truncation at floor 3", gaps)
	}
	if len(r.seqs()) != 0 {
		t.Fatalf("applied %v, skipping rows 1..2", r.seqs())
	}
}

func TestBaselineStartsAtTheHeadWithAFingerprint(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	r := &recorder{}
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap}
	cp, err := tl.Baseline(context.Background())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if cp.Seq != 3 || cp.Last.Seq != 3 || cp.Last != fingerprintOf(j.rows[2]) {
		t.Fatalf("Baseline = %+v", cp)
	}
	j.append("d")
	mustStep(t, tl)
	if got, want := r.seqs(), []int64{4}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
}

func TestBaselineOnAnEmptyJournal(t *testing.T) {
	r := &recorder{}
	tl := &Tailer{Source: &fakeJournal{}, Apply: r.apply, OnGap: r.onGap}
	cp, err := tl.Baseline(context.Background())
	if err != nil || cp != (Checkpoint{}) {
		t.Fatalf("Baseline = (%+v, %v), want the zero checkpoint", cp, err)
	}
}

func TestStepRefusesAnUnconfiguredTailer(t *testing.T) {
	r := &recorder{}
	for name, tl := range map[string]*Tailer{
		"no Source": {Apply: r.apply, OnGap: r.onGap},
		"no Apply":  {Source: &fakeJournal{}, OnGap: r.onGap},
		"no OnGap":  {Source: &fakeJournal{}, Apply: r.apply},
	} {
		if _, err := tl.Step(context.Background()); err == nil {
			t.Errorf("Step with %s succeeded", name)
		}
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
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap, PageLimit: 2, Sleep: clock.sleep}

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
	ctx, cancel := context.WithCancel(context.Background())
	clock := &scriptedSleep{after: 5, cancel: cancel}
	r := &recorder{}
	tl := &Tailer{Source: &fakeJournal{}, Apply: r.apply, OnGap: r.onGap, Sleep: clock.sleep}
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
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap, Sleep: clock.sleep, OnError: func(err error) error {
		reported = append(reported, err)
		return nil
	}}
	_ = tl.Run(ctx)
	if len(reported) != 1 {
		t.Fatalf("OnError saw %v, want one apply failure", reported)
	}
	if got, want := r.seqs(), []int64{1}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v after the retry", got, want)
	}
}

func TestRunStopsWhenOnErrorSaysSo(t *testing.T) {
	permanent := errors.New("journal refused for good")
	j := &fakeJournal{failErr: permanent}
	r := &recorder{}
	// The clock ends a Run that ignored the stop, so the test fails on an
	// assertion instead of hanging until the package timeout.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &scriptedSleep{after: 3, cancel: cancel}
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap, Sleep: clock.sleep, OnError: func(err error) error {
		if errors.Is(err, permanent) {
			return err
		}
		return nil
	}}
	if err := tl.Run(ctx); !errors.Is(err, permanent) {
		t.Fatalf("Run = %v, want the permanent refusal", err)
	}
	if len(clock.delays) != 0 {
		t.Fatalf("Run slept %v before stopping, want it to stop on the first refusal", clock.delays)
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &recorder{}
	tl := &Tailer{Source: &fakeJournal{}, Apply: r.apply, OnGap: r.onGap, Poll: time.Millisecond, IdlePoll: time.Millisecond}
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

// flappingJournal renders every row's TS differently on alternate reads, as
// replicas with different normalization behind one address would. Every
// verification read then sees a different fingerprint and reports a reset.
type flappingJournal struct {
	mu     sync.Mutex
	rows   []journalops.Row
	reads  int
	cap    int
	cancel context.CancelFunc
}

func (f *flappingJournal) ReadEventsJournalPage(_ context.Context, since int64, limit int) (journalops.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.reads >= f.cap {
		// A hot loop never sleeps, so the clock never stops it; this does.
		f.cancel()
	}
	head := f.rows[len(f.rows)-1].Seq
	var out []journalops.Row
	for _, r := range f.rows {
		if r.Seq > since && (limit == 0 || len(out) < limit) {
			r.TS = fmt.Sprintf("%s-%d", r.TS, f.reads%2)
			out = append(out, r)
		}
	}
	return journalops.Page{Rows: out, Head: head}, nil
}

func TestRunBacksOffAcrossConsecutiveGaps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &flappingJournal{
		rows:   []journalops.Row{{Seq: 1, IssueID: "a", TS: "t1"}, {Seq: 2, IssueID: "b", TS: "t2"}},
		cap:    1000,
		cancel: cancel,
	}
	r := &recorder{}
	clock := &scriptedSleep{after: 5, cancel: cancel}
	tl := &Tailer{Source: src, Apply: r.apply, OnGap: r.onGap, Sleep: clock.sleep}

	_ = tl.Run(ctx)
	if src.reads >= src.cap {
		t.Fatalf("Run read %d times with %d sleeps: consecutive gaps re-read at once without backing off", src.reads, len(clock.delays))
	}
	if len(clock.delays) != clock.after {
		t.Fatalf("sleeps = %v, want %d", clock.delays, clock.after)
	}
	// At most one immediate re-read per sleep: a gap re-reads at once only
	// when the read before it was not a gap too.
	if gaps := len(r.gapList()); gaps == 0 || gaps > len(clock.delays)+1 {
		t.Fatalf("OnGap ran %d times across %d sleeps, want between 1 and %d", gaps, len(clock.delays), len(clock.delays)+1)
	}
	// The backoff keeps growing across the run of gaps instead of restarting.
	if last := clock.delays[len(clock.delays)-1]; last != DefaultIdlePoll {
		t.Fatalf("delays = %v, want the backoff to reach %v", clock.delays, DefaultIdlePoll)
	}
}

// cappedJournal serves at most cap rows per read whatever limit asks for, as
// a server with its own page ceiling does.
type cappedJournal struct {
	*fakeJournal
	cap int
}

func (c *cappedJournal) ReadEventsJournalPage(ctx context.Context, since int64, limit int) (journalops.Page, error) {
	if limit == 0 || limit > c.cap {
		limit = c.cap
	}
	return c.fakeJournal.ReadEventsJournalPage(ctx, since, limit)
}

// A Source that caps pages below PageLimit still leaves the head ahead, so
// Step must ask for an immediate re-read rather than sleep between pages.
func TestStepReportsMoreWhileTheHeadIsAheadOfACappedPage(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c", "d")
	r := &recorder{}
	tl := &Tailer{Source: &cappedJournal{fakeJournal: j, cap: 2}, Apply: r.apply, OnGap: r.onGap, PageLimit: 10}

	if !mustStep(t, tl) {
		t.Fatal("a capped page short of the head did not ask for an immediate re-read")
	}
	if got, want := r.seqs(), []int64{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
}

// A Source capped at one row spends every verified read on the checkpoint
// row. That must surface as an error, not as an idle tailer that never moves
// while the head runs away.
func TestStepReportsASourceTooNarrowToVerify(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b", "c")
	r := &recorder{}
	tl := &Tailer{Source: &cappedJournal{fakeJournal: j, cap: 1}, Apply: r.apply, OnGap: r.onGap}

	mustStep(t, tl) // applies row 1; the checkpoint is now verifiable
	more, err := tl.Step(context.Background())
	if err == nil {
		t.Fatalf("Step = (%v, nil) with the head at 3 and the checkpoint stuck at %d", more, tl.Checkpoint().Seq)
	}
	if more {
		t.Fatal("a failed Step asked for an immediate re-read")
	}
	if stats := tl.Stats(); stats.Errors != 1 || stats.Head != 3 {
		t.Fatalf("stats = %+v, want one error and the head observed", stats)
	}
}

func mustResume(t *testing.T, tl *Tailer, cp Checkpoint) {
	t.Helper()
	if err := tl.Resume(cp); err != nil {
		t.Fatalf("Resume(%+v): %v", cp, err)
	}
}

func TestResumeRefusesANegativeSeq(t *testing.T) {
	r := &recorder{}
	tl := &Tailer{Source: &fakeJournal{}, Apply: r.apply, OnGap: r.onGap}
	if err := tl.Resume(Checkpoint{Seq: -1}); err == nil {
		t.Fatal("Resume accepted seq -1, which no journal serves")
	}
	if got := tl.Checkpoint(); got != (Checkpoint{}) {
		t.Fatalf("checkpoint = %+v after a refused Resume", got)
	}
}

// Resume waits for an in-flight Step, so the Step's commit cannot overwrite
// the checkpoint the caller just restored.
func TestResumeIsNotOverwrittenByAnInFlightStep(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	entered, release := make(chan struct{}), make(chan struct{})
	tl := &Tailer{Source: j, OnGap: (&recorder{}).onGap, Apply: func(context.Context, journalops.Page) error {
		close(entered)
		<-release
		return nil
	}}

	stepped := make(chan error, 1)
	go func() {
		_, err := tl.Step(context.Background())
		stepped <- err
	}()
	<-entered
	restored := Checkpoint{Seq: 1, Last: Fingerprint{Seq: 1, IssueID: "a", TS: "ts-1-a"}}
	resumed := make(chan error, 1)
	go func() { resumed <- tl.Resume(restored) }()
	select {
	case err := <-resumed:
		t.Fatalf("Resume returned (%v) while a Step was applying", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-stepped; err != nil {
		t.Fatalf("Step: %v", err)
	}
	if err := <-resumed; err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := tl.Checkpoint(); got != restored {
		t.Fatalf("checkpoint = %+v, want the resumed %+v", got, restored)
	}
}

// With no OnError, Run retries every failure; the cause must still be
// visible, not only a count.
func TestStatsKeepTheLastErrorAndWhenItHappened(t *testing.T) {
	boom := errors.New("connection reset")
	j := &fakeJournal{failErr: boom}
	j.append("a")
	r := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	clock := &scriptedSleep{after: 1, cancel: cancel}
	tl := &Tailer{Source: j, Apply: r.apply, OnGap: r.onGap, Sleep: clock.sleep}

	before := time.Now()
	_ = tl.Run(ctx)
	stats := tl.Stats()
	if stats.Errors != 1 || !errors.Is(stats.LastError, boom) {
		t.Fatalf("stats = %+v, want one error whose cause is %v", stats, boom)
	}
	if stats.LastErrorAt.Before(before) {
		t.Fatalf("LastErrorAt = %v, want it stamped when the error happened (after %v)", stats.LastErrorAt, before)
	}
}

// rebaselineFailJournal reports truncation on every page read and refuses
// every fingerprint read (limit 1), so the re-baseline after each gap fails.
type rebaselineFailJournal struct {
	mu     sync.Mutex
	reads  int
	cap    int
	cancel context.CancelFunc
}

func (j *rebaselineFailJournal) ReadEventsJournalPage(_ context.Context, since int64, limit int) (journalops.Page, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.reads++
	if j.cap > 0 && j.reads >= j.cap {
		// A hot loop never sleeps, so the clock never stops it; this does.
		j.cancel()
	}
	if limit == 1 {
		return journalops.Page{}, errRebaselineRefused
	}
	return journalops.Page{}, &journalops.TruncatedError{Since: since, Floor: 50, Head: 60}
}

var errRebaselineRefused = errors.New("fingerprint read refused")

// A gap whose re-baseline read fails is a failed Step: it keeps the
// checkpoint and reports more=false, so Run waits before the next attempt
// instead of re-reading in a hot loop.
func TestStepReportsNoMoreWhenTheRebaselineReadFails(t *testing.T) {
	r := &recorder{}
	tl := &Tailer{Source: &rebaselineFailJournal{}, Apply: r.apply, OnGap: r.onGap}
	more, err := tl.Step(context.Background())
	if !errors.Is(err, errRebaselineRefused) {
		t.Fatalf("Step err = %v, want the refused re-baseline read", err)
	}
	if more {
		t.Fatal("Step reported more=true after a failed re-baseline read")
	}
	if cp := tl.Checkpoint(); cp != (Checkpoint{}) {
		t.Fatalf("checkpoint = %+v, want it unmoved", cp)
	}
	if gaps := r.gapList(); len(gaps) != 0 {
		t.Fatalf("OnGap ran %d times, want none before a baseline exists", len(gaps))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &rebaselineFailJournal{cap: 1000, cancel: cancel}
	clock := &scriptedSleep{after: 5, cancel: cancel}
	tl = &Tailer{Source: src, Apply: r.apply, OnGap: r.onGap, Sleep: clock.sleep}
	_ = tl.Run(ctx)
	if src.reads >= src.cap {
		t.Fatalf("Run read %d times with %d sleeps: a failed re-baseline re-read at once", src.reads, len(clock.delays))
	}
	if len(clock.delays) != clock.after {
		t.Fatalf("sleeps = %v, want %d", clock.delays, clock.after)
	}
}

// errOff stands in for a journal's refusal while it records nothing; a gc
// consumer's Lapse matches beads.ErrEventsJournalInactive instead.
var errOff = errors.New("journal is not recording")

func lapseOn(err error) bool { return errors.Is(err, errOff) }

// Writes made while the journal was off have no seq, so the rows after the
// lapse continue the checkpoint and no other check can see the hole. The
// refusal is the only evidence, and the read after it must be a gap.
func TestAnActivationLapseIsAGap(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	r := &recorder{}
	tl := newTailer(j, r)
	tl.Lapse = lapseOn
	mustStep(t, tl) // applies 1..2

	j.failErr = errors.New("connection reset")
	if _, err := tl.Step(context.Background()); err == nil {
		t.Fatal("Step succeeded through a failed read")
	}
	if cp := tl.Checkpoint(); cp.Lapsed {
		t.Fatalf("checkpoint = %+v: an error Lapse does not match marked a lapse", cp)
	}

	j.failErr = fmt.Errorf("reading journal after seq 1: %w", errOff)
	if more, err := tl.Step(context.Background()); !errors.Is(err, errOff) || more {
		t.Fatalf("Step = (%v, %v), want the refusal and no re-read", more, err)
	}
	if cp := tl.Checkpoint(); !cp.Lapsed || cp.Seq != 2 {
		t.Fatalf("checkpoint = %+v, want seq 2 marked lapsed", cp)
	}

	// The journal is back on; c and d are journaled, and whatever was written
	// while it was off is not.
	j.append("c", "d")
	r.gapFailN = 1
	if _, err := tl.Step(context.Background()); err == nil {
		t.Fatal("Step succeeded though the resync failed")
	}
	if cp := tl.Checkpoint(); !cp.Lapsed || cp.Seq != 2 {
		t.Fatalf("checkpoint = %+v after a failed resync, want the lapse kept", cp)
	}
	if !mustStep(t, tl) {
		t.Fatal("a gap must ask for an immediate re-read")
	}
	gaps := r.gapList()
	if len(gaps) != 1 || gaps[0].Kind != GapInactive || gaps[0].From.Seq != 2 || !gaps[0].From.Lapsed ||
		gaps[0].Head != 4 || gaps[0].To.Seq != 4 || gaps[0].To.Lapsed {
		t.Fatalf("gaps = %+v, want one inactive gap from lapsed seq 2 to head 4", gaps)
	}
	if got, want := r.seqs(), []int64{1, 2}; !slices.Equal(got, want) {
		t.Fatalf("applied %v over the lapse, want %v: rows 3..4 are the consumer's resync", got, want)
	}

	j.append("e")
	mustStep(t, tl)
	if got, want := r.seqs(), []int64{1, 2, 5}; !slices.Equal(got, want) {
		t.Fatalf("applied %v, want %v", got, want)
	}
	if stats := tl.Stats(); stats.Gaps[GapInactive] != 1 || len(stats.Gaps) != 1 {
		t.Fatalf("stats gaps = %+v, want one inactive gap", stats.Gaps)
	}
}

// The lapse travels with the checkpoint. A consumer that persisted it after
// the refused Step and restarted still resyncs, even with no row journaled
// since: the writes the lapse lost have no rows at all.
func TestALapsedCheckpointResumesLapsed(t *testing.T) {
	j := &fakeJournal{}
	j.append("a", "b")
	first := newTailer(j, &recorder{})
	first.Lapse = lapseOn
	mustStep(t, first)
	j.failErr = errOff
	if _, err := first.Step(context.Background()); !errors.Is(err, errOff) {
		t.Fatalf("Step = %v, want the refusal", err)
	}
	saved := first.Checkpoint()

	r := &recorder{}
	tl := newTailer(j, r) // no Lapse: the persisted mark carries the lapse
	mustResume(t, tl, saved)
	mustStep(t, tl)
	if gaps := r.gapList(); len(gaps) != 1 || gaps[0].Kind != GapInactive || gaps[0].From != saved || gaps[0].To.Seq != 2 {
		t.Fatalf("gaps = %+v, want one inactive gap from %+v", gaps, saved)
	}
	if cp := tl.Checkpoint(); cp.Lapsed || cp.Last.Seq != 2 {
		t.Fatalf("checkpoint = %+v, want seq 2 fingerprinted and the lapse cleared", cp)
	}
}

// Every read can see a lapse, Baseline's included. A Baseline that succeeds
// clears one: the consumer's scan after it covers what the lapse lost.
func TestBaselineSeesAndClearsALapse(t *testing.T) {
	j := &fakeJournal{}
	j.append("a")
	r := &recorder{}
	tl := newTailer(j, r)
	tl.Lapse = lapseOn
	j.failErr = errOff
	if _, err := tl.Baseline(context.Background()); !errors.Is(err, errOff) {
		t.Fatalf("Baseline = %v, want the refusal", err)
	}
	if cp := tl.Checkpoint(); !cp.Lapsed {
		t.Fatalf("checkpoint = %+v, want the refused Baseline read to mark a lapse", cp)
	}
	cp, err := tl.Baseline(context.Background())
	if err != nil || cp.Lapsed || cp.Seq != 1 || tl.Checkpoint() != cp {
		t.Fatalf("Baseline = (%+v, %v), want seq 1 with the lapse cleared", cp, err)
	}
	j.append("b")
	mustStep(t, tl)
	if gaps, got := r.gapList(), r.seqs(); len(gaps) != 0 || !slices.Equal(got, []int64{2}) {
		t.Fatalf("gaps = %+v, applied %v; want no gap and row 2", gaps, got)
	}
}
