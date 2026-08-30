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

// droppingJournal wraps a durable fake but SILENTLY DROPS Record for one chosen
// subject, reproducing the best-effort FileRecorder.Record failure the stamp
// must tolerate (closed recorder, cross-process flock timeout, write error).
// Because the drop never reaches the log, List never returns that fact — so a
// warm re-derivation cannot re-confirm it, which is exactly the phantom the
// convergence stamp must not treat as a verified witness. It embeds the
// events.Provider INTERFACE (not *Fake) so it does not accidentally satisfy
// events.InFlightProvider and completedFacts takes its List branch.
type droppingJournal struct {
	events.Provider
	dropSubject string
	dropped     int
}

func (j *droppingJournal) Record(e events.Event) {
	if e.Subject == j.dropSubject {
		j.dropped++
		return
	}
	j.Provider.Record(e)
}

// assertNotStamped fails if the root carries a converged stamp.
func assertNotStamped(t *testing.T, store beads.Store, rootID, msg string) {
	t.Helper()
	root, err := store.Get(rootID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] != "" {
		t.Fatalf("%s: root %s is stamped converged", msg, rootID)
	}
}

// TestCompletionBackstopStampsOnlyVerifiedConvergenceThenSkips: an EMITTING
// pass must never stamp — its Record calls are fire-and-forget, and a stamp in
// the same pass would turn a silently dropped append into a permanent fact
// loss. The root converges one sweep later, when every fact reads back from
// the journal, and is skipped forever after.
func TestCompletionBackstopStampsOnlyVerifiedConvergenceThenSkips(t *testing.T) {
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
		if root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] != "" {
			t.Fatalf("root %s stamped by the pass that EMITTED its facts; a dropped Record would now be a permanent loss", id)
		}
	}

	second := &CompletionBackstop{}
	r2 := second.Pass(journal, stores, "execution-reconcile")
	if !r2.SweepComplete || r2.Emitted != 0 || r2.RootsVisited != 3 {
		t.Fatalf("second sweep = %+v, want a verifying visit of all 3 roots", r2)
	}
	for _, id := range rootIDs {
		root, err := backing.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] == "" {
			t.Fatalf("verified-converged root %s not stamped", id)
		}
	}

	stepListsBefore := store.stepLists
	third := &CompletionBackstop{}
	r3 := third.Pass(journal, stores, "execution-reconcile")
	if !r3.SweepComplete || r3.Emitted != 0 || r3.RootsSkippedConverged != 3 {
		t.Fatalf("third sweep = %+v, want 3 stamped roots skipped", r3)
	}
	if store.stepLists != stepListsBefore {
		t.Fatalf("third sweep issued %d per-root step listing(s) for stamped roots, want 0", store.stepLists-stepListsBefore)
	}
}

// TestCompletionBackstopDoesNotStampAnEmptyStepListing: a store wedge that
// answers empty-with-nil must not vacuously prove convergence.
func TestCompletionBackstopDoesNotStampAnEmptyStepListing(t *testing.T) {
	backing := beads.NewMemStore()
	root := mustCreateProjectionRoot(t, backing, "")
	closed := "closed"
	if err := backing.Update(root.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: backing}}

	for range 2 {
		(&CompletionBackstop{}).Pass(journal, stores, "execution-reconcile")
	}
	after, err := backing.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] != "" {
		t.Fatal("a root with ZERO listed steps was stamped converged; an empty listing proves nothing")
	}
}

// TestCompletionBackstopStartupSweepRevisitsAndClearsStaleStamps: the stamp is
// a cadence optimization, not a terminal verdict — the per-boot startup sweep
// re-examines stamped roots and clears a stamp whose root can still emit (a
// hand-reopened and re-closed step), so a wrong stamp is bounded by one boot.
func TestCompletionBackstopStartupSweepRevisitsAndClearsStaleStamps(t *testing.T) {
	backing, rootIDs, stepIDs := closedCompletionCorpus(t, 1)
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: backing}}

	for range 2 {
		(&CompletionBackstop{}).Pass(journal, stores, "execution-reconcile")
	}
	root, err := backing.Get(rootIDs[0])
	if err != nil || root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] == "" {
		t.Fatalf("seed stamping failed: %v", err)
	}

	// Incident surgery: a step is hand-reopened and re-closes under a NEW
	// session id — a new emittable fact key the delta lane never hears about.
	open, closed := "open", "closed"
	if err := backing.Update(stepIDs[0], beads.UpdateOpts{Status: &open}); err != nil {
		t.Fatal(err)
	}
	if err := backing.Update(stepIDs[0], beads.UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{beadmeta.SessionIDMetadataKey: "gcs-resurrected"},
	}); err != nil {
		t.Fatal(err)
	}

	// Cadence sweeps skip the stamped root: the fact stays owed.
	cadence := &CompletionBackstop{}
	if r := cadence.Pass(journal, stores, "execution-reconcile"); r.Emitted != 0 || r.RootsSkippedConverged != 1 {
		t.Fatalf("cadence sweep = %+v, want the stamped root skipped", r)
	}

	// The startup sweep revisits, emits the owed fact, and CLEARS the stamp.
	startup := &CompletionBackstop{VisitStamped: true}
	if r := startup.Pass(journal, stores, "execution-reconcile"); r.Emitted != 1 || r.RootsSkippedConverged != 0 {
		t.Fatalf("startup sweep = %+v, want the owed fact emitted", r)
	}
	after, err := backing.Get(rootIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] != "" {
		t.Fatal("the startup sweep left a STALE stamp on a root it just emitted for")
	}
}

// TestCompletionBackstopClearsStampWhenStampedRootReopens: the converged stamp's
// SOLE purpose is to let sweeps SKIP a root, so a stale stamp on a root that has
// REOPENED silently voids the backstop's recovery guarantee for it — a later
// dropped step-completion fact under that reopened root would never be
// re-emitted. A root can converge (closed + every fact journal-confirmed), get
// stamped, and then reopen (root.Status → open) while every step row stays
// closed (workflow re-drive/retry). Neither the stamp branch (needs !stamped)
// nor the OLD clear branch (no root.Status term) fired for that shape, and the
// cadence filter skipped the still-stamped root outright — so the stamp survived
// for the rest of the process lifetime AND across reboots. Both the cadence
// sweep and the startup sweep must revisit the reopened root and clear its stamp.
func TestCompletionBackstopClearsStampWhenStampedRootReopens(t *testing.T) {
	// seedReopenedStampedRoot returns a store whose single root has been stamped
	// converged and then reopened with every step row still closed. It returns
	// the SAME journal the stamping used, so every step-completion fact is
	// already journal-confirmed on the healing pass: emittedForRoot==0 and
	// unconfirmedForRoot==0, leaving the reopened root.Status as the ONLY thing
	// that can clear the stamp. A fresh/empty journal would re-emit the facts and
	// clear via emittedForRoot>0, masking the exact regression under test.
	seedReopenedStampedRoot := func(t *testing.T) (beads.Store, *events.Fake, string) {
		t.Helper()
		backing, rootIDs, _ := closedCompletionCorpus(t, 1)
		journal := events.NewFake()
		stores := []beads.GraphStore{{Store: backing}}
		// First sweep emits the facts; the second reads them back and stamps.
		for range 2 {
			(&CompletionBackstop{}).Pass(journal, stores, "execution-reconcile")
		}
		root, err := backing.Get(rootIDs[0])
		if err != nil || root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] == "" {
			t.Fatalf("seed stamping failed: %v", err)
		}
		// The converged, stamped root REOPENS while every step row stays closed.
		open := "open"
		if err := backing.Update(rootIDs[0], beads.UpdateOpts{Status: &open}); err != nil {
			t.Fatal(err)
		}
		return backing, journal, rootIDs[0]
	}

	t.Run("cadence sweep heals a reopened stamped root", func(t *testing.T) {
		backing, journal, rootID := seedReopenedStampedRoot(t)
		stores := []beads.GraphStore{{Store: backing}}
		r := (&CompletionBackstop{}).Pass(journal, stores, "execution-reconcile")
		if r.Emitted != 0 {
			t.Fatalf("cadence sweep = %+v, want no re-emit (facts already confirmed); the test must isolate the reopened-root.Status clear", r)
		}
		if r.RootsSkippedConverged != 0 || r.RootsVisited != 1 {
			t.Fatalf("cadence sweep = %+v, want the reopened root VISITED, not skipped as converged", r)
		}
		assertNotStamped(t, backing, rootID, "a cadence sweep left a STALE stamp on a reopened root; the backstop now permanently skips it")
	})

	t.Run("startup sweep heals a reopened stamped root", func(t *testing.T) {
		backing, journal, rootID := seedReopenedStampedRoot(t)
		stores := []beads.GraphStore{{Store: backing}}
		r := (&CompletionBackstop{VisitStamped: true}).Pass(journal, stores, "execution-reconcile")
		if r.Emitted != 0 {
			t.Fatalf("startup sweep = %+v, want no re-emit (facts already confirmed)", r)
		}
		if r.RootsSkippedConverged != 0 || r.RootsVisited != 1 {
			t.Fatalf("startup sweep = %+v, want the reopened root visited", r)
		}
		assertNotStamped(t, backing, rootID, "the startup sweep left a STALE stamp on a reopened root")
	})
}

// TestCompletionBackstopDoesNotStampLiveRoots: an open root can still grow and
// close steps, so it is revisited every sweep.
func TestCompletionBackstopDoesNotStampLiveRoots(t *testing.T) {
	backing, rootIDs, _ := completionCorpus(t, 1)
	store := &sweepCountingGraphStore{Store: backing}
	journal := events.NewFake()
	stores := []beads.GraphStore{{Store: store}}

	for i := range 3 {
		r := (&CompletionBackstop{}).Pass(journal, stores, "execution-reconcile")
		if r.RootsVisited != 1 || r.RootsSkippedConverged != 0 {
			t.Fatalf("sweep %d = %+v, want the live root revisited every sweep", i, r)
		}
	}
	root, err := backing.Get(rootIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if root.Metadata[beadmeta.CompletionFactsConvergedMetadataKey] != "" {
		t.Fatal("an OPEN root was stamped converged; its steps can still change")
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

// TestCompletionBackstopDoesNotStampFromAnUnconfirmedPhantom reuses ONE backstop
// across sweeps — exactly as the lane does (completions_lane.go allocates it
// outside the sweep loop) — against a journal that permanently drops the owed
// fact's Record. Each emitting pass add()s a key the journal never received; on
// the OLD code the retained phantom made the SECOND sweep read has(key)=true,
// emittedForRoot==0, and STAMP the root converged, permanently masking the lost
// close (every later cadence sweep then skips the stamped root). The fix records
// a self-add()ed key as UNCONFIRMED, so the start-of-sweep re-derivation purges
// it and the owed fact is re-attempted every sweep instead of masked — and no
// unconfirmed witness is ever allowed to stamp the root.
func TestCompletionBackstopDoesNotStampFromAnUnconfirmedPhantom(t *testing.T) {
	backing, rootIDs, stepIDs := closedCompletionCorpus(t, 1)
	journal := &droppingJournal{Provider: events.NewFake(), dropSubject: stepIDs[0]}
	stores := []beads.GraphStore{{Store: backing}}

	// One long-lived backstop, reused across every sweep below.
	backstop := &CompletionBackstop{}

	// Every sweep must RE-ATTEMPT the dropped fact (Emitted 1) and must never
	// stamp the root: the only witness is this process's own unconfirmed add,
	// which the journal never received. On the old code the second sweep emits 0
	// and stamps — this loop fails there and passes after the fix.
	for sweep := 1; sweep <= 3; sweep++ {
		r := backstop.Pass(journal, stores, "execution-reconcile")
		if r.Emitted != 1 {
			t.Fatalf("sweep %d = %+v, want the dropped fact re-attempted (Emitted 1); a stamp/skip here masks the lost close", sweep, r)
		}
		assertNotStamped(t, backing, rootIDs[0], "a root whose only witness is an unconfirmed phantom was stamped converged; the dropped close is now a permanent loss")
	}
	if journal.dropped != 3 {
		t.Fatalf("journal dropped %d record(s) over 3 sweeps, want 3 — the fixture is not exercising the drop each pass", journal.dropped)
	}
}
