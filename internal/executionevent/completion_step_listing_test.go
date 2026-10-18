package executionevent

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// stepListingCountingStore counts the step reads a sweep issues: the per-root
// listings (gc.root_bead_id=<root>), the one-read listings (every row carrying
// gc.root_bead_id), and the converged-stamp writes. failKeyedList makes the
// one-read listing fail, the way a store that cannot answer it would.
type stepListingCountingStore struct {
	beads.Store
	perRootLists  int
	keyedLists    int
	stampWrites   int
	failKeyedList bool
}

func (s *stepListingCountingStore) ListByMetadata(filter map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if _, ok := filter[beadmeta.RootBeadIDMetadataKey]; ok {
		s.perRootLists++
	}
	return s.Store.ListByMetadata(filter, limit, opts...)
}

func (s *stepListingCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.HasMetadataKey == beadmeta.RootBeadIDMetadataKey {
		s.keyedLists++
		if s.failKeyedList {
			return nil, errors.New("store cannot answer a has-key listing")
		}
	}
	return s.Store.List(q)
}

func (s *stepListingCountingStore) SetMetadata(id, key, value string) error {
	if key == beadmeta.CompletionFactsConvergedMetadataKey {
		s.stampWrites++
	}
	return s.Store.SetMetadata(id, key, value)
}

// convergedClosedCorpus returns n closed roots, each with one closed step,
// whose facts are journaled and whose stamps are written: the long-finished
// shape that is nearly every root of a real store.
func convergedClosedCorpus(t *testing.T, n int) (beads.Store, *events.Fake, []string, []string) {
	t.Helper()
	backing, rootIDs, stepIDs := closedCompletionCorpus(t, n)
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: backing}}
	// The first sweep records the facts; the second reads them back and stamps.
	for range 2 {
		(&CompletionBackstop{}).Pass(journal, stores, "execution-reconcile")
	}
	for _, id := range rootIDs {
		assertStamped(t, backing, id)
	}
	return backing, journal, rootIDs, stepIDs
}

func assertStamped(t *testing.T, store beads.Store, rootID string) {
	t.Helper()
	root, err := store.Get(rootID)
	if err != nil || root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] == "" {
		t.Fatalf("root %s not stamped converged (err %v)", rootID, err)
	}
}

// TestStartupSweepListsStepsOncePerStore: the startup sweep visits every root,
// and listing each root's steps on its own made it one store-wide read per root
// ever created (8,107 on one large store). One listing of every step
// row settles the roots that need nothing, so a converged store costs one step
// read, not one per root, and the sweep's outcome is unchanged.
func TestStartupSweepListsStepsOncePerStore(t *testing.T) {
	const n = 12
	backing, journal, rootIDs, _ := convergedClosedCorpus(t, n)
	store := &stepListingCountingStore{Store: backing}
	stores := []beads.GraphStore{{Store: store}}

	r := (&CompletionBackstop{VisitStamped: true}).Pass(journal, stores, "execution-reconcile")
	if r.RootsVisited != n || r.Emitted != 0 || len(r.StepListErrors) != 0 {
		t.Fatalf("startup sweep = %+v, want all %d roots visited, nothing emitted, no step-list error", r, n)
	}
	if store.perRootLists != 0 || store.keyedLists != 1 {
		t.Fatalf("startup sweep made %d per-root step listing(s) and %d one-read listing(s), want 0 and 1", store.perRootLists, store.keyedLists)
	}
	if store.stampWrites != 0 {
		t.Fatalf("startup sweep wrote %d stamp(s) on a converged store, want 0", store.stampWrites)
	}
	for _, id := range rootIDs {
		assertStamped(t, backing, id)
	}

	// A chunked startup sweep holds the one listing for the whole store.
	store.perRootLists, store.keyedLists = 0, 0
	chunked := &CompletionBackstop{VisitStamped: true, ChunkSize: 5}
	for {
		r := chunked.Pass(journal, stores, "execution-reconcile")
		if r.SweepComplete {
			break
		}
	}
	if store.perRootLists != 0 || store.keyedLists != 1 {
		t.Fatalf("chunked startup sweep made %d per-root and %d one-read listing(s), want 0 and 1", store.perRootLists, store.keyedLists)
	}
}

// TestStartupSweepOfSettledRootsIsNotPacedByTheChunk: a root the one listing
// settled costs a map lookup, so it is not charged to the chunk. Charging it
// paced a converged city at one chunk interval per ChunkSize roots, about 70
// minutes of waiting for about 9,000 roots that needed nothing. A converged
// store larger than the chunk now finishes its startup sweep in one Pass.
func TestStartupSweepOfSettledRootsIsNotPacedByTheChunk(t *testing.T) {
	const n, chunk = 12, 5
	backing, journal, _, stepIDs := convergedClosedCorpus(t, n)
	store := &stepListingCountingStore{Store: backing}
	stores := []beads.GraphStore{{Store: store}}

	r := (&CompletionBackstop{VisitStamped: true, ChunkSize: chunk}).Pass(journal, stores, "execution-reconcile")
	if !r.SweepComplete || r.RootsVisited != n || r.RootsSettled != n || r.Emitted != 0 {
		t.Fatalf("startup sweep = %+v, want one complete Pass with all %d roots visited and settled", r, n)
	}
	if store.perRootLists != 0 || store.keyedLists != 1 {
		t.Fatalf("startup sweep made %d per-root and %d one-read listing(s), want 0 and 1", store.perRootLists, store.keyedLists)
	}

	// Control: roots that still need their own listing are still capped by the
	// chunk. Reopen-and-reclose more steps than the chunk holds, so those roots
	// owe a fact the one listing cannot settle.
	open, closed := "open", "closed"
	const owed = chunk + 2
	for i := range owed {
		if err := backing.Update(stepIDs[i], beads.UpdateOpts{Status: &open}); err != nil {
			t.Fatal(err)
		}
		if err := backing.Update(stepIDs[i], beads.UpdateOpts{
			Status:   &closed,
			Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-resurrected"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	store.perRootLists, store.keyedLists = 0, 0
	backstop := &CompletionBackstop{VisitStamped: true, ChunkSize: chunk}
	visited, settled, emitted, passes := 0, 0, 0, 0
	for {
		passes++
		r := backstop.Pass(journal, stores, "execution-reconcile")
		if listed := r.RootsVisited - r.RootsSettled; listed > chunk {
			t.Fatalf("pass %d listed %d root(s) on their own, want at most the chunk size %d", passes, listed, chunk)
		}
		visited += r.RootsVisited
		settled += r.RootsSettled
		emitted += r.Emitted
		if r.SweepComplete {
			break
		}
	}
	if passes != 2 || visited != n || settled != n-owed || emitted != owed {
		t.Fatalf("startup sweep took %d pass(es), visited %d, settled %d, emitted %d; want 2, %d, %d, %d", passes, visited, settled, emitted, n, n-owed, owed)
	}
	if store.perRootLists != owed {
		t.Fatalf("startup sweep made %d per-root listing(s), want %d (the owed roots only)", store.perRootLists, owed)
	}
}

// TestStartupSweepRereadsOnlyRootsThatNeedAWrite: a root the one listing cannot
// settle is read again by its own listing before anything is recorded or
// written, so the fact and the stamp are still decided from rows read then.
func TestStartupSweepRereadsOnlyRootsThatNeedAWrite(t *testing.T) {
	const n = 6
	backing, journal, rootIDs, stepIDs := convergedClosedCorpus(t, n)

	// A step is hand-reopened and re-closes under a new session id: a fact the
	// journal does not hold, under a root that is still stamped.
	open, closed := "open", "closed"
	if err := backing.Update(stepIDs[2], beads.UpdateOpts{Status: &open}); err != nil {
		t.Fatal(err)
	}
	if err := backing.Update(stepIDs[2], beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-resurrected"},
	}); err != nil {
		t.Fatal(err)
	}

	store := &stepListingCountingStore{Store: backing}
	stores := []beads.GraphStore{{Store: store}}
	r := (&CompletionBackstop{VisitStamped: true}).Pass(journal, stores, "execution-reconcile")
	if r.RootsVisited != n || r.Emitted != 1 {
		t.Fatalf("startup sweep = %+v, want %d roots visited and the owed fact emitted", r, n)
	}
	if store.keyedLists != 1 || store.perRootLists != 1 {
		t.Fatalf("startup sweep made %d one-read and %d per-root listing(s), want 1 and 1 (the owed root only)", store.keyedLists, store.perRootLists)
	}
	after, err := backing.Get(rootIDs[2])
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] != "" {
		t.Fatal("the startup sweep left a stale stamp on the root it emitted for")
	}
	for i, id := range rootIDs {
		if i != 2 {
			assertStamped(t, backing, id)
		}
	}
}

// TestStartupSweepFallsBackWhenTheOneListingFails: a store that cannot answer
// the one listing skips nothing. Every root is listed on its own, as before
// this listing existed, and the sweep says so.
func TestStartupSweepFallsBackWhenTheOneListingFails(t *testing.T) {
	const n = 4
	backing, journal, rootIDs, _ := convergedClosedCorpus(t, n)
	store := &stepListingCountingStore{Store: backing, failKeyedList: true}
	stores := []beads.GraphStore{{Store: store}}

	r := (&CompletionBackstop{VisitStamped: true}).Pass(journal, stores, "execution-reconcile")
	if r.RootsVisited != n || r.Emitted != 0 || len(r.ListErrors) != 0 {
		t.Fatalf("startup sweep = %+v, want all %d roots visited and no store skipped", r, n)
	}
	if len(r.StepListErrors) != 1 {
		t.Fatalf("startup sweep reported %d step-list error(s), want 1: %v", len(r.StepListErrors), r.StepListErrors)
	}
	if store.perRootLists != n {
		t.Fatalf("startup sweep made %d per-root listing(s) after the one listing failed, want %d", store.perRootLists, n)
	}
	for _, id := range rootIDs {
		assertStamped(t, backing, id)
	}
}

// TestCadenceSweepKeepsPerRootListings: a cadence sweep visits only the roots
// that are not converged, so it keeps their own listings and makes no
// store-wide step read.
func TestCadenceSweepKeepsPerRootListings(t *testing.T) {
	backing, rootIDs, _ := completionCorpus(t, 3)
	store := &stepListingCountingStore{Store: backing}
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: store}}

	r := (&CompletionBackstop{}).Pass(journal, stores, "execution-reconcile")
	if r.RootsVisited != len(rootIDs) {
		t.Fatalf("cadence sweep = %+v, want %d roots visited", r, len(rootIDs))
	}
	if store.keyedLists != 0 || store.perRootLists != len(rootIDs) {
		t.Fatalf("cadence sweep made %d one-read and %d per-root listing(s), want 0 and %d", store.keyedLists, store.perRootLists, len(rootIDs))
	}
}
