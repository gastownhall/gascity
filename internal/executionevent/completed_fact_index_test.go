package executionevent

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// countingJournal counts the journal reads a pass issues. A completion pass's
// idempotency record used to be rebuilt from the journal on EVERY pass that
// named a root, and on maintainer-city that is every tick: 69.7s of a 373s tick,
// spent re-deriving a set that had not changed (ga-l7jdg).
//
// It deliberately does NOT implement events.InFlightProvider, so completedFacts
// takes its List branch and the counter sees every read.
type countingJournal struct {
	events.Provider
	reads      int
	latestSeqs int
}

func (j *countingJournal) List(filter events.Filter) ([]events.Event, error) {
	j.reads++
	return j.Provider.List(filter)
}

func (j *countingJournal) LatestSeq() (uint64, error) {
	j.latestSeqs++
	return j.Provider.LatestSeq()
}

// completionCorpus seeds n graph.v2 roots, each with one closed step. It returns
// the root ids and the step ids in the same order, so a test can name the exact
// close whose fact it is reasoning about.
func completionCorpus(t *testing.T, n int) (beads.Store, []string, []string) {
	t.Helper()
	backing := beads.NewMemStore()
	closed := "closed"
	rootIDs := make([]string, 0, n)
	stepIDs := make([]string, 0, n)
	for i := range n {
		root := mustCreateProjectionRoot(t, backing, "")
		rootIDs = append(rootIDs, root.ID)
		step := mustCreateProjectionStep(t, backing, stepBeadID(i, 0), root.ID, "build", "[]")
		stepIDs = append(stepIDs, step.ID)
		if err := backing.Update(step.ID, beads.UpdateOpts{
			Status:   &closed,
			Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-session"},
		}); err != nil {
			t.Fatalf("close step %s: %v", step.ID, err)
		}
	}
	return backing, rootIDs, stepIDs
}

// TestCompletedFactIndexReadsTheJournalOnceAndThenNever is the property the
// runtime never had: a tick that names roots every time must not re-read the
// journal every time.
func TestCompletedFactIndexReadsTheJournalOnceAndThenNever(t *testing.T) {
	store, rootIDs, _ := completionCorpus(t, 4)
	journal := &countingJournal{Provider: events.NewFake()}
	stores := []beads.GraphStore{{Store: store}}
	var index CompletedFactIndex

	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != len(rootIDs) {
		t.Fatalf("first pass emitted %d, want %d — the budget below would be met by a pass that repairs nothing", emitted, len(rootIDs))
	}
	if journal.reads != 1 {
		t.Fatalf("warming the index cost %d journal read(s), want 1", journal.reads)
	}
	for range 5 {
		if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 0 {
			t.Fatalf("a repeat pass emitted %d fact(s); the index is not remembering what it recorded", emitted)
		}
	}
	if journal.reads != 1 || journal.latestSeqs != 0 {
		t.Fatalf("5 further passes cost %d journal read(s) and %d head read(s), want 1 and 0 in total", journal.reads, journal.latestSeqs)
	}
}

// TestCompletedFactIndexAbsorbsFactsFromTheFeedInsteadOfRereading: the journal
// feed the delta lane already tails is this index's cursor. A fact another
// writer appended must suppress the recovery fact WITHOUT a journal read.
func TestCompletedFactIndexAbsorbsFactsFromTheFeedInsteadOfRereading(t *testing.T) {
	store, rootIDs, stepIDs := completionCorpus(t, 1)
	fake := events.NewFake()
	journal := &countingJournal{Provider: fake}
	stores := []beads.GraphStore{{Store: store}}
	var index CompletedFactIndex

	// Warm the index while the journal is still empty, then let the close path
	// record the fact this pass would otherwise have to invent.
	index.ReconcileRoots(journal, stores, nil, "execution-reconcile") // no roots: no warm-up
	if journal.reads != 0 {
		t.Fatalf("a pass with no named roots read the journal %d time(s), want 0", journal.reads)
	}
	index.ReconcileRoots(journal, stores, []string{"gcg-absent"}, "execution-reconcile")
	if journal.reads != 1 {
		t.Fatalf("the warm-up read the journal %d time(s), want 1", journal.reads)
	}

	root, err := store.Get(rootIDs[0])
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	step, err := store.Get(stepIDs[0])
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	fact, ok := LifecycleEvent(events.ExecutionStepCompleted, root, step, "close-path")
	if !ok {
		t.Fatal("the fixture step does not produce a lifecycle fact; the assertion below would be vacuous")
	}
	fake.Record(fact)
	index.Absorb(fact)

	before := len(fake.Events)
	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("the pass emitted %d duplicate fact(s) for a fact the feed already named", emitted)
	}
	if journal.reads != 1 {
		t.Fatalf("the pass read the journal %d time(s) in total, want 1 — the feed is supposed to be the cursor", journal.reads)
	}
	if len(fake.Events) != before {
		t.Fatalf("the journal grew from %d to %d events on a pass that emitted nothing", before, len(fake.Events))
	}

	// Control: an UNabsorbed close still gets its recovery fact, so the
	// suppression above is idempotency and not a lane that stopped emitting.
	store2, roots2, _ := completionCorpus(t, 1)
	var fresh CompletedFactIndex
	if emitted := fresh.ReconcileRoots(&countingJournal{Provider: events.NewFake()}, []beads.GraphStore{{Store: store2}}, roots2, "execution-reconcile"); emitted != 1 {
		t.Fatalf("an unrecorded close emitted %d fact(s), want 1", emitted)
	}
}

// TestCompletedFactIndexInvalidateRebuildsFromTheJournal: a feed that can no
// longer promise to name every event cannot keep this record current either, so
// the gap hook drops it and the next pass pays one read to be sure.
func TestCompletedFactIndexInvalidateRebuildsFromTheJournal(t *testing.T) {
	store, rootIDs, _ := completionCorpus(t, 1)
	journal := &countingJournal{Provider: events.NewFake()}
	stores := []beads.GraphStore{{Store: store}}
	var index CompletedFactIndex

	index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile")
	if journal.reads != 1 {
		t.Fatalf("warm-up cost %d read(s), want 1", journal.reads)
	}
	index.Invalidate()
	if emitted := index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("the rebuilt index emitted %d duplicate fact(s); the rebuild did not see the journal it just wrote", emitted)
	}
	if journal.reads != 2 {
		t.Fatalf("an invalidated index cost %d journal read(s) in total, want 2", journal.reads)
	}
}

// TestCompletedFactIndexRefusesToEmitOnAnUnreadableJournal: without the
// idempotency record a pass would duplicate every recovery fact, so it declines
// and a later pass retries.
func TestCompletedFactIndexRefusesToEmitOnAnUnreadableJournal(t *testing.T) {
	store, rootIDs, _ := completionCorpus(t, 1)
	broken := events.NewFailFake()
	var index CompletedFactIndex
	if emitted := index.ReconcileRoots(broken, []beads.GraphStore{{Store: store}}, rootIDs, "execution-reconcile"); emitted != 0 {
		t.Fatalf("emitted %d fact(s) with no readable idempotency record, want 0", emitted)
	}
	// Control: the same corpus over a readable journal DOES emit, so the zero
	// above is the refusal and not an empty fixture.
	if emitted := (&CompletedFactIndex{}).ReconcileRoots(events.NewFake(), []beads.GraphStore{{Store: store}}, rootIDs, "execution-reconcile"); emitted != 1 {
		t.Fatalf("emitted %d fact(s) over a readable journal, want 1", emitted)
	}
}

// TestCompletionBackstopLoadsOncePerSweepNotPerChunk: chunking exists to bound
// one convergence pass, and re-reading the whole journal per chunk would make the
// bound cost more than the sweep it bounds. A NEW sweep must still re-derive the
// record, or it would only know the facts it emitted itself.
func TestCompletionBackstopLoadsOncePerSweepNotPerChunk(t *testing.T) {
	store, _, _ := completionCorpus(t, 5)
	journal := &countingJournal{Provider: events.NewFake()}
	stores := []beads.GraphStore{{Store: store}}
	backstop := &CompletionBackstop{ChunkSize: 2}

	var chunks, emitted int
	for {
		result := backstop.Pass(journal, stores, "execution-reconcile")
		chunks++
		emitted += result.Emitted
		if result.SweepComplete {
			break
		}
		if chunks > 10 {
			t.Fatal("the sweep never completed; the cursor is not advancing")
		}
	}
	if chunks < 3 {
		t.Fatalf("a 5-root corpus at chunk size 2 took %d chunk(s); the assertion below is not measuring chunking", chunks)
	}
	if emitted != 5 {
		t.Fatalf("the sweep emitted %d fact(s) over 5 closed steps, want 5", emitted)
	}
	if journal.reads != 1 {
		t.Fatalf("a %d-chunk sweep cost %d journal read(s), want 1", chunks, journal.reads)
	}

	// The NEXT sweep re-derives: nothing feeds this index between sweeps, so a
	// warm one would re-emit everything another writer recorded meanwhile.
	backstop.Pass(journal, stores, "execution-reconcile")
	if journal.reads != 2 {
		t.Fatalf("a second sweep cost %d journal read(s) in total, want 2 — a convergence lane that never re-reads converges against its own memory", journal.reads)
	}
}

// TestCompletedFactIndexGrowthBoundDoesNotRebuildEveryPass is the bound's own
// failure mode, pinned.
//
// The bound has to be on GROWTH, not on absolute size. A city whose journal
// already retains more facts than the cap would, under an absolute bound, exceed
// it the moment it loaded — and rebuild on every single pass, reinstating the
// O(retained-history) read per tick this type exists to delete.
func TestCompletedFactIndexGrowthBoundDoesNotRebuildEveryPass(t *testing.T) {
	fake := events.NewFake()
	for i := range completedFactIndexGrowthCap + 10 {
		fake.Record(events.Event{
			Type:    events.ExecutionStepCompleted,
			Subject: "gcg-step-" + itoa(i),
			RunID:   "gcg-root-" + itoa(i),
			StepID:  "build",
		})
	}
	journal := &countingJournal{Provider: fake}
	store, rootIDs, _ := completionCorpus(t, 1)
	stores := []beads.GraphStore{{Store: store}}
	var index CompletedFactIndex

	index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile")
	if journal.reads != 1 {
		t.Fatalf("warm-up cost %d journal read(s), want 1", journal.reads)
	}
	// Control: the fixture really did overshoot any absolute cap, so the
	// assertion below is testing the relative bound and not an empty journal.
	if len(index.facts) <= completedFactIndexGrowthCap {
		t.Fatalf("the loaded index holds %d fact(s), which does not exceed the cap of %d; the fixture proves nothing", len(index.facts), completedFactIndexGrowthCap)
	}
	for range 5 {
		index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile")
	}
	if journal.reads != 1 {
		t.Fatalf("5 passes over an oversized journal cost %d read(s) in total, want 1 — an absolute cap would rebuild on every one", journal.reads)
	}

	// And the bound still fires on real GROWTH past the loaded baseline.
	index.mu.Lock()
	for i := range completedFactIndexGrowthCap + 1 {
		index.facts[completedFactKey{subject: "grown-" + itoa(i)}] = true
	}
	index.mu.Unlock()
	index.ReconcileRoots(journal, stores, rootIDs, "execution-reconcile")
	if journal.reads != 2 {
		t.Fatalf("a set grown a full cap past its baseline cost %d read(s) in total, want 2 — the bound never fires and the index is a leak", journal.reads)
	}
	// The over-cap pass must REBUILD, not merely re-floor the baseline: the
	// injected keys the journal never held are dropped, so the set shrinks back
	// toward what retention holds instead of leaking one key per fact ever seen.
	if len(index.facts) >= 2*completedFactIndexGrowthCap {
		t.Fatalf("after the over-cap pass the set still holds %d fact(s); a real rebuild drops the injected keys the journal never backed, a bare baseline re-floor does not", len(index.facts))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
