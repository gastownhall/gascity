package executionevent

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// TestCompletionBackstopRestartsWhenStoreFanReordersMidSweep pins the store-set
// identity guard. The sweep cursor is an INDEX into the caller's store slice,
// and the caller re-resolves that slice fresh on every chunk
// (completionReconcileInputs rebuilds [graph, city, ...sorted rigs], so a rig
// add+remove or a rig-store pointer replacement reorders the tail WITHOUT
// changing its length). A guard that restarts only on a length CHANGE misses
// the equal-length reorder: the cursor then holds one store's roots while
// pointing at a different store, and the displaced store's roots are never
// listed that sweep.
func TestCompletionBackstopRestartsWhenStoreFanReordersMidSweep(t *testing.T) {
	backingA, _, _ := completionCorpus(t, 2)
	backingB, _, _ := completionCorpus(t, 2)
	storeA := &sweepCountingGraphStore{Store: backingA}
	storeB := &sweepCountingGraphStore{Store: backingB}
	journal := events.NewFake()

	// ChunkSize 1 leaves the cursor inside store A (index 0) after the first
	// chunk, so the reorder below lands mid-sweep with A's roots still held.
	backstop := &CompletionBackstop{ChunkSize: 1}
	ordered := []beads.GraphStore{{Store: storeA}, {Store: storeB}}
	reordered := []beads.GraphStore{{Store: storeB}, {Store: storeA}}

	stores := ordered
	for i := 0; ; i++ {
		r := backstop.Pass(journal, stores, "execution-reconcile")
		if i == 0 {
			// An equal-length permutation of the fan, applied under the
			// in-progress sweep exactly once.
			stores = reordered
		}
		if r.SweepComplete {
			break
		}
		if i > 20 {
			t.Fatal("the sweep never completed; the cursor is not advancing")
		}
	}

	// The displaced store must still have had its roots listed during the sweep
	// that saw the reorder. Under a length-only guard it is skipped entirely
	// (rootLists 0) and its completion facts are deferred a whole sweep cycle.
	if storeB.rootLists == 0 {
		t.Fatal("store B's roots were never listed in the sweep that saw an equal-length fan reorder; the length-only guard skipped a whole store")
	}
	if storeA.rootLists == 0 {
		t.Fatal("control: store A was never listed either, so the sweep did not run and the assertion above is vacuous")
	}
}

// TestCompletionBackstopGrowthCapFiresOnTheSweepPath pins the sweep-path form of
// the growth bound. The backstop Invalidate()s its index at the start of every
// sweep (loaded=false), so the cap must fire even though the index is cold at
// the sweep's warm. On any city whose sweep completes in one chunk (ChunkSize
// 64, i.e. <=64 workflow roots, the common case) the index is ONLY ever warmed
// right after an Invalidate; if the cap is gated on loaded it is structurally
// unreachable there, and confirmed keys for facts that have rotated out of
// journal retention accumulate without bound over the controller's lifetime.
func TestCompletionBackstopGrowthCapFiresOnTheSweepPath(t *testing.T) {
	store, _, _ := completionCorpus(t, 1)
	journal := &filterRecordingJournal{Provider: events.NewFake()}
	stores := []beads.GraphStore{{Store: store}}
	backstop := &CompletionBackstop{} // whole sweep in one chunk

	countFullReads := func() int {
		n := 0
		for _, f := range journal.filters {
			if f.AfterSeq == 0 {
				n++
			}
		}
		return n
	}

	// Reach steady state. The corpus's own fact is emitted on the first sweep and
	// read back on the second, so the high-water settles after a couple of
	// sweeps; from there each sweep re-derives from the journal TAIL.
	for range 5 {
		if r := backstop.Pass(journal, stores, "execution-reconcile"); !r.SweepComplete {
			t.Fatalf("single-chunk sweep did not complete: %+v", r)
		}
	}

	// A steady-state sweep must NOT re-read the whole journal: holding the index
	// warm across sweeps is the entire point, and a naive "rebuild every sweep"
	// fix would reinstate the O(retained-history) read this type deletes.
	fullReadsSteady := countFullReads()
	if r := backstop.Pass(journal, stores, "execution-reconcile"); !r.SweepComplete {
		t.Fatalf("steady-state sweep did not complete: %+v", r)
	}
	if got := countFullReads(); got != fullReadsSteady {
		t.Fatalf("a steady-state sweep issued a full-history read (AfterSeq=0): total went %d -> %d; the warm index must tail-read", fullReadsSteady, got)
	}

	// Simulate a long-lived controller: confirmed keys for facts that have since
	// rotated OUT of journal retention accumulate in the warm set across sweeps.
	// Inject a full cap-plus-one past the load baseline. These keys are NOT in
	// the journal, so only a real rebuild (full re-read) can drop them.
	backstop.index.mu.Lock()
	baseline := backstop.index.baseline
	for i := range completedFactIndexGrowthCap + 1 {
		backstop.index.facts[completedFactKey{subject: "rotated-" + itoa(i)}] = true
	}
	grown := len(backstop.index.facts)
	backstop.index.mu.Unlock()
	if grown <= baseline+completedFactIndexGrowthCap {
		t.Fatalf("fixture grew the set to %d, not past baseline %d + cap %d; the assertion would be vacuous", grown, baseline, completedFactIndexGrowthCap)
	}

	// The next sweep must REBUILD despite the sweep-start Invalidate resetting
	// loaded: a full re-read (AfterSeq=0) that drops the injected keys the
	// journal never held, trimming the set back to journal-retained reality.
	fullReadsBefore := countFullReads()
	over := backstop.Pass(journal, stores, "execution-reconcile")
	if !over.SweepComplete {
		t.Fatalf("over-cap sweep did not complete: %+v", over)
	}
	if over.Emitted != 0 {
		t.Fatalf("the over-cap rebuild emitted %d fact(s); the corpus fact is journal-confirmed and must stay deduped through a rebuild", over.Emitted)
	}
	if got := countFullReads(); got != fullReadsBefore+1 {
		t.Fatalf("the over-cap sweep issued %d full-history read(s) total, want %d — the growth cap never fired on the sweep path and the index leaks unbounded", got, fullReadsBefore+1)
	}
	backstop.index.mu.Lock()
	trimmed := len(backstop.index.facts)
	backstop.index.mu.Unlock()
	if trimmed >= grown {
		t.Fatalf("after the over-cap sweep the set still holds %d fact(s) (was %d); a real rebuild drops the injected keys the journal never backed", trimmed, grown)
	}
	if trimmed > completedFactIndexGrowthCap {
		t.Fatalf("after the rebuild the set holds %d fact(s), still over the cap; the rotated-out keys were not dropped", trimmed)
	}
}
