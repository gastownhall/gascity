package executionevent

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// filterRecordingJournal records the exact Filter of every List read, so a test
// can assert not just HOW OFTEN the journal was read but HOW MUCH of it each
// read asked for. Like countingJournal it deliberately does not implement
// events.InFlightProvider, so completedFacts takes its List branch.
type filterRecordingJournal struct {
	events.Provider
	filters []events.Filter
}

func (j *filterRecordingJournal) List(filter events.Filter) ([]events.Event, error) {
	j.filters = append(j.filters, filter)
	return j.Provider.List(filter)
}

// TestCompletedFactIndexRewarmReadsOnlyTheJournalTail pins the cost shape of a
// re-derivation. Invalidate exists so a new sweep re-reads what OTHER writers
// recorded since the last one — but journal seqs are monotonic and a fact once
// read cannot change, so the re-read must start at the index's high-water, not
// at byte 0. On maintainer-city the full-history form of this read (every
// archive gunzipped plus the whole active journal, per sweep chunk retry, per
// city) was the primary supervisor I/O burn (~330MB/s, ga-ftgyl).
func TestCompletedFactIndexRewarmReadsOnlyTheJournalTail(t *testing.T) {
	store, rootIDs, _ := completionCorpus(t, 1)
	journal := &filterRecordingJournal{Provider: events.NewFake()}
	stores := []beads.GraphStore{{Store: store}}
	var index CompletedFactIndex

	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 1 {
		t.Fatalf("first pass emitted %d fact(s), want 1", emitted)
	}
	if len(journal.filters) == 0 {
		t.Fatal("the first pass performed no journal read at all; the assertions below measure nothing")
	}
	if first := journal.filters[0]; first.AfterSeq != 0 {
		t.Fatalf("the COLD load read AfterSeq=%d, want 0 — a cold index has no high-water to resume from", first.AfterSeq)
	}
	coldReads := len(journal.filters)

	// First re-derivation: the cold load saw an EMPTY journal (the fact was
	// emitted after it), so its high-water is legitimately zero and this read
	// starts at zero. It is the read that establishes the high-water.
	index.Invalidate()
	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("the re-derived index emitted %d duplicate fact(s)", emitted)
	}
	if len(journal.filters) <= coldReads {
		t.Fatal("the invalidated index performed no re-read; Invalidate no longer re-derives")
	}
	warmReads := len(journal.filters)

	// Steady state: every re-derivation after a successful read resumes at the
	// high-water. This is the read that repeats for the life of the process.
	index.Invalidate()
	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("the steady-state re-derivation emitted %d duplicate fact(s)", emitted)
	}
	if len(journal.filters) <= warmReads {
		t.Fatal("the second invalidation performed no re-read")
	}
	for _, f := range journal.filters[warmReads:] {
		if f.AfterSeq == 0 {
			t.Fatalf("a steady-state re-derivation read the journal from AfterSeq=0; a warm index must resume from its high-water (filters=%+v)", journal.filters)
		}
	}
}

// TestCompletedFactIndexWarmErrorKeepsSetAndHighWater: a failed re-read refuses
// the pass (correctness), but it must not throw away what a previous successful
// read established — or every retry after a transient journal error pays the
// full-history read again, which is latch 1 of ga-ftgyl.
func TestCompletedFactIndexWarmErrorKeepsSetAndHighWater(t *testing.T) {
	store, rootIDs, _ := completionCorpus(t, 1)
	good := &filterRecordingJournal{Provider: events.NewFake()}
	stores := []beads.GraphStore{{Store: store}}
	var index CompletedFactIndex

	if emitted := index.ReconcileRoots(good, stores, rootIDs, "execution-reconcile"); emitted != 1 {
		t.Fatalf("seed pass emitted %d fact(s), want 1", emitted)
	}
	// Establish the high-water: this re-read sees the fact the seed pass
	// recorded, so every read after it can resume.
	index.Invalidate()
	if emitted := index.ReconcileRoots(good, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("high-water pass emitted %d duplicate fact(s)", emitted)
	}
	index.Invalidate()
	if emitted := index.ReconcileRoots(events.NewFailFake(), stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("emitted %d fact(s) over an unreadable journal, want 0", emitted)
	}
	readsBefore := len(good.filters)
	if emitted := index.ReconcileRoots(good, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("post-error pass emitted %d duplicate fact(s); the error dropped the set", emitted)
	}
	for _, f := range good.filters[readsBefore:] {
		if f.AfterSeq == 0 {
			t.Fatalf("the post-error re-read started at AfterSeq=0; the error discarded the high-water (filters=%v)", good.filters)
		}
	}
}

// TestCompletionBackstopPassReportsWarmFailure: a chunk that could not warm its
// idempotency record converged nothing, and the caller has to be able to tell
// that apart from a budget-bounded chunk — the completions lane re-polls a
// budget chunk immediately but must back off a warm failure, or it re-issues
// the full-history read every poll interval forever (latch 2 of ga-ftgyl).
func TestCompletionBackstopPassReportsWarmFailure(t *testing.T) {
	store, _, _ := completionCorpus(t, 1)
	stores := []beads.GraphStore{{Store: store}}

	backstop := &CompletionBackstop{ChunkSize: 1}
	result := backstop.Pass(events.NewFailFake(), stores, "execution-reconcile")
	if !result.WarmFailed {
		t.Fatalf("Pass over an unreadable journal reported WarmFailed=%t, want true", result.WarmFailed)
	}
	if result.SweepComplete {
		t.Fatal("a warm failure must not report the sweep complete")
	}

	// Control: a readable journal never reports WarmFailed.
	good := &CompletionBackstop{ChunkSize: 1}
	if r := good.Pass(events.NewFake(), stores, "execution-reconcile"); r.WarmFailed {
		t.Fatal("control failed: a readable journal reported WarmFailed")
	}
}
