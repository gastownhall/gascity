package beadstest

import (
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunAtomicConditionalCloserConformance runs the store-agnostic contract of
// beads.AtomicConditionalCloser against a capable store: the metadata merge and
// the close commit together behind the revision fence, or nothing changes.
// open must return a fresh, empty store whose capability is discoverable
// through beads.AtomicConditionalCloserFor; name prefixes every subtest so
// several stores can run in one package.
//
// Terminal writers (session close) depend on exactly these properties: a close
// that lands carries its terminal metadata in the same revision, and a close
// that loses the fence leaves the row as it was so the caller can re-read and
// retry.
func RunAtomicConditionalCloserConformance(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Helper()
	atomicCloseMergesAndClosesInOneRevision(t, name, open)
	atomicCloseNeverMutatedBeadAtItsReadRevision(t, name, open)
	atomicCloseStaleRevisionLeavesRowUntouched(t, name, open)
	atomicCloseMissingBeadIsNotFound(t, name, open)
	atomicCloseSameRevisionRacersHaveOneWinner(t, name, open)
}

func atomicCloserFor(t *testing.T, s beads.Store) beads.AtomicConditionalCloser {
	t.Helper()
	closer, ok := beads.AtomicConditionalCloserFor(s)
	if !ok {
		t.Fatalf("AtomicConditionalCloserFor(%T) = unavailable, want the atomic terminal close", s)
	}
	return closer
}

func atomicCloseFixture(t *testing.T, s beads.Store, title string) beads.Bead {
	t.Helper()
	created, err := s.Create(beads.Bead{Title: title})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetMetadataBatch(created.ID, map[string]string{"keep": "kept", "state": "asleep"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	before, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return before
}

func atomicCloseMergesAndClosesInOneRevision(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/merges_metadata_and_closes_in_one_revision", func(t *testing.T) {
		s := open(t)
		closer := atomicCloserFor(t, s)
		before := atomicCloseFixture(t, s, "atomic-close")

		closed, err := closer.CloseWithMetadataIfMatch(before.ID, before.Revision, map[string]string{
			"state":        "drained",
			"close_reason": "terminal",
		})
		if err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
		if closed.Status != "closed" || closed.Metadata["state"] != "drained" ||
			closed.Metadata["close_reason"] != "terminal" || closed.Metadata["keep"] != "kept" {
			t.Fatalf("returned row = status %q metadata %v, want closed with the merge applied over the kept keys", closed.Status, closed.Metadata)
		}
		if closed.Revision == 0 || closed.Revision == before.Revision {
			t.Fatalf("returned revision = %d (before %d), want one fresh nonzero revision", closed.Revision, before.Revision)
		}

		// The returned row must be a copy: scribbling on it cannot reach the store.
		closed.Metadata["state"] = "corrupted caller copy"

		after, err := s.Get(before.ID)
		if err != nil {
			t.Fatalf("Get after close: %v", err)
		}
		if after.Status != "closed" || after.Metadata["state"] != "drained" ||
			after.Metadata["close_reason"] != "terminal" || after.Metadata["keep"] != "kept" {
			t.Fatalf("persisted row = status %q metadata %v, want the terminal close and its metadata", after.Status, after.Metadata)
		}
		if after.Revision != closed.Revision {
			t.Fatalf("persisted revision = %d, returned %d: the close must be one mutation", after.Revision, closed.Revision)
		}
	})
}

// atomicCloseNeverMutatedBeadAtItsReadRevision covers a row closed straight
// after create (a failed-create cleanup). Its read revision may be zero on a
// counter-backed store (the revision contract allows that), and a close fenced
// on exactly what Get returned must still land.
func atomicCloseNeverMutatedBeadAtItsReadRevision(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/closes_a_never_mutated_bead_at_its_read_revision", func(t *testing.T) {
		s := open(t)
		closer := atomicCloserFor(t, s)
		created, err := s.Create(beads.Bead{Title: "atomic-close-fresh"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		observed, err := s.Get(created.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		closed, err := closer.CloseWithMetadataIfMatch(observed.ID, observed.Revision, map[string]string{"state": "failed-create"})
		if err != nil {
			t.Fatalf("CloseWithMetadataIfMatch at read revision %d: %v", observed.Revision, err)
		}
		if closed.Status != "closed" || closed.Metadata["state"] != "failed-create" {
			t.Fatalf("returned row = status %q metadata %v, want closed/failed-create", closed.Status, closed.Metadata)
		}
	})
}

func atomicCloseStaleRevisionLeavesRowUntouched(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/stale_revision_leaves_row_untouched", func(t *testing.T) {
		s := open(t)
		closer := atomicCloserFor(t, s)
		observed := atomicCloseFixture(t, s, "atomic-close-stale")

		// A concurrent writer lands after the observation: the canonical
		// "wake stamps state=awake" interleaving.
		if err := s.SetMetadata(observed.ID, "state", "awake"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		current, err := s.Get(observed.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		_, err = closer.CloseWithMetadataIfMatch(observed.ID, observed.Revision, map[string]string{"state": "drained"})
		if !beads.IsPreconditionFailed(err) {
			t.Fatalf("stale CloseWithMetadataIfMatch = %v, want *PreconditionFailedError", err)
		}
		after, err := s.Get(observed.ID)
		if err != nil {
			t.Fatalf("Get after refused close: %v", err)
		}
		if after.Status != current.Status || after.Metadata["state"] != "awake" || after.Revision != current.Revision {
			t.Fatalf("refused close mutated the row: status %q state %q revision %d, want %q %q %d",
				after.Status, after.Metadata["state"], after.Revision, current.Status, "awake", current.Revision)
		}
	})
}

func atomicCloseMissingBeadIsNotFound(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/missing_bead_is_not_found", func(t *testing.T) {
		s := open(t)
		closer := atomicCloserFor(t, s)
		_, err := closer.CloseWithMetadataIfMatch("missing-atomic-close", 1, map[string]string{"state": "drained"})
		if !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("CloseWithMetadataIfMatch(missing) = %v, want ErrNotFound", err)
		}
	})
}

func atomicCloseSameRevisionRacersHaveOneWinner(t *testing.T, name string, open func(t *testing.T) beads.Store) {
	t.Run(name+"/same_revision_racers_have_one_winner", func(t *testing.T) {
		s := open(t)
		closer := atomicCloserFor(t, s)
		before := atomicCloseFixture(t, s, "atomic-close-race")

		const racers = 4
		errs := make([]error, racers)
		start := make(chan struct{})
		var done sync.WaitGroup
		for i := range racers {
			done.Add(1)
			go func() {
				defer done.Done()
				<-start
				_, errs[i] = closer.CloseWithMetadataIfMatch(before.ID, before.Revision, map[string]string{"state": "drained"})
			}()
		}
		close(start)
		done.Wait()

		wins := 0
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
			case beads.IsPreconditionFailed(err):
			default:
				t.Fatalf("racer %d: %v, want success or *PreconditionFailedError", i, err)
			}
		}
		if wins != 1 {
			t.Fatalf("same-revision atomic closes had %d winners, want exactly 1", wins)
		}
	})
}
