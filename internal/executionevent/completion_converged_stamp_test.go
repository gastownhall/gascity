package executionevent

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// countingGraphStore counts the metadata listings a sweep issues, split by
// shape: root listings (kind=workflow) versus per-root step listings
// (gc.root_bead_id=...). The completions sweep's cost story is exactly these
// two counters — 917 step listings per sweep per city was 7m41s of store
// reads to rediscover a converged state every hour (ga-wevcl).
type sweepCountingGraphStore struct {
	beads.Store
	rootLists int
	stepLists int
}

func (s *sweepCountingGraphStore) ListByMetadata(filter map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if _, ok := filter[beadmeta.RootBeadIDMetadataKey]; ok {
		s.stepLists++
	}
	if filter[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
		s.rootLists++
	}
	return s.Store.ListByMetadata(filter, limit, opts...)
}

// closedCompletionCorpus is completionCorpus with the roots ALSO closed — the
// long-finished-molecule shape that dominates a real store.
func closedCompletionCorpus(t *testing.T, n int) (beads.Store, []string, []string) {
	t.Helper()
	backing, rootIDs, stepIDs := completionCorpus(t, n)
	closed := "closed"
	for _, id := range rootIDs {
		if err := backing.Update(id, beads.UpdateOpts{Status: &closed}); err != nil {
			t.Fatalf("close root %s: %v", id, err)
		}
	}
	return backing, rootIDs, stepIDs
}

// TestCompletionBackstopStampsAndSkipsConvergedRoots: a closed root whose
// every step is closed and whose every fact is journaled cannot change again,
// so the first sweep that proves that stamps it, and later sweeps skip it
// without a per-root step listing.
func TestCompletionBackstopStampsAndSkipsConvergedRoots(t *testing.T) {
	backing, rootIDs, _ := closedCompletionCorpus(t, 3)
	store := &sweepCountingGraphStore{Store: backing}
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: store}}

	first := &CompletionBackstop{}
	r1 := first.Pass(journal, stores, "execution-reconcile")
	if !r1.SweepComplete || r1.Emitted != 3 || r1.RootsVisited != 3 {
		t.Fatalf("first sweep = %+v, want complete with 3 emitted over 3 roots", r1)
	}
	for _, id := range rootIDs {
		root, err := backing.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] == "" {
			t.Fatalf("converged root %s not stamped", id)
		}
	}

	stepListsAfterFirst := store.stepLists
	second := &CompletionBackstop{}
	r2 := second.Pass(journal, stores, "execution-reconcile")
	if !r2.SweepComplete || r2.Emitted != 0 {
		t.Fatalf("second sweep = %+v, want complete with 0 emitted", r2)
	}
	if r2.RootsSkippedConverged != 3 {
		t.Fatalf("second sweep skipped %d converged root(s), want 3", r2.RootsSkippedConverged)
	}
	if store.stepLists != stepListsAfterFirst {
		t.Fatalf("second sweep issued %d per-root step listing(s) for stamped roots, want 0", store.stepLists-stepListsAfterFirst)
	}
}

// TestCompletionBackstopDoesNotStampLiveRoots: an open root can still grow and
// close steps, so it is revisited every sweep.
func TestCompletionBackstopDoesNotStampLiveRoots(t *testing.T) {
	backing, rootIDs, _ := completionCorpus(t, 1)
	store := &sweepCountingGraphStore{Store: backing}
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: store}}

	first := &CompletionBackstop{}
	if r := first.Pass(journal, stores, "execution-reconcile"); r.Emitted != 1 {
		t.Fatalf("first sweep emitted %d, want 1", r.Emitted)
	}
	root, err := backing.Get(rootIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] != "" {
		t.Fatal("an OPEN root was stamped converged; its steps can still change")
	}

	second := &CompletionBackstop{}
	if r := second.Pass(journal, stores, "execution-reconcile"); r.RootsVisited != 1 || r.RootsSkippedConverged != 0 {
		t.Fatalf("second sweep = %+v, want the live root revisited", r)
	}
}

// TestCompletionBackstopHoldsRootListForOneSweep: the chunked sweep used to
// re-list and re-sort every store's full root set on every chunk — O(roots^2)
// listing work per sweep. One sweep pays one root listing per store.
func TestCompletionBackstopHoldsRootListForOneSweep(t *testing.T) {
	backing, _, _ := completionCorpus(t, 5)
	store := &sweepCountingGraphStore{Store: backing}
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: store}}
	backstop := &CompletionBackstop{ChunkSize: 2}

	chunks := 0
	for {
		result := backstop.Pass(journal, stores, "execution-reconcile")
		chunks++
		if result.SweepComplete {
			break
		}
		if chunks > 10 {
			t.Fatal("sweep never completed")
		}
	}
	if chunks < 3 {
		t.Fatalf("5 roots at chunk 2 took %d chunk(s); not exercising chunking", chunks)
	}
	if store.rootLists != 1 {
		t.Fatalf("a %d-chunk sweep issued %d root listing(s), want 1 — the root list must be held for the sweep", chunks, store.rootLists)
	}
}
