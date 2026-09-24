package beads

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// closeReopenSpy serves an issue whose status can flip mid-retry, so a replayed
// attempt re-reads current state rather than a frozen snapshot. That is the
// whole point: Close and Reopen re-read inside the attempt, so a replay decides
// from what the store holds now.
//
// The mutate callback controls the status flip independently of the returned
// error, which lets a test model a concurrent writer closing the bead BETWEEN
// attempts. Read that word literally: the callback runs synchronously inside
// the failing attempt, before it returns its conflict, so these tests do not
// exercise backoff timing and deleting the sleep would not turn them red. What
// they do establish is the property that matters, that a replay re-reads and
// short-circuits on state another actor left behind. Testing the timing itself
// would need an injectable backoff, which is not worth building for this.
//
// Note what these tests deliberately do NOT model: a conflict reported after
// this store's own write committed. That cannot happen on this path, because
// every CloseIssue branch commits exactly once (permanent, wisp, and embedded
// alike). See closeOnce's comment in native_dolt_store.go for the per-branch
// argument; do not restate it here, since the two copies drifted once already.
type closeReopenSpy struct {
	*nativeDoltStorageSpy
	gets    int32
	mutates int32
}

// newCloseSpy returns a spy whose issue starts open. closeIssue runs mutate,
// which decides what that attempt does to the stored status and what it
// returns, so a test can model "a concurrent actor closed it before the replay"
// as easily as "this attempt lost the race and nothing landed".
func newCloseSpy(mutate func(attempt int32, markClosed func()) error) *closeReopenSpy {
	spy := &closeReopenSpy{}
	closed := int32(0)
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			atomic.AddInt32(&spy.gets, 1)
			status := beadslib.StatusOpen
			if atomic.LoadInt32(&closed) == 1 {
				status = beadslib.StatusClosed
			}
			return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2}, nil
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			n := atomic.AddInt32(&spy.mutates, 1)
			return mutate(n, func() { atomic.StoreInt32(&closed, 1) })
		},
	}
	return spy
}

func TestNativeDoltStoreCloseRetriesSerializationConflict(t *testing.T) {
	spy := newCloseSpy(func(attempt int32, markClosed func()) error {
		if attempt == 1 {
			// Lost the race: nothing landed.
			return errors.New(serializationConflictErr)
		}
		markClosed()
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Close("gc-1"); err != nil {
		t.Fatalf("Close after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 2 {
		t.Fatalf("CloseIssue attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

// TestNativeDoltStoreCloseReplayShortCircuitsWhenAnotherActorClosedTheBead
// covers why the re-read has to sit INSIDE the retried unit. This store loses
// the race and a different writer closes the bead before the replay runs. The
// replay must notice that and not issue a second close.
//
// The assertion that carries the weight is the CloseIssue count. Hoisting the
// read out of the retry would leave the replay deciding from the first
// snapshot, which still said open, and the count would be 2. The GetIssue
// count is what proves the re-read actually happened rather than the status
// short-circuit being reached some other way.
func TestNativeDoltStoreCloseReplayShortCircuitsWhenAnotherActorClosedTheBead(t *testing.T) {
	spy := newCloseSpy(func(attempt int32, markClosed func()) error {
		if attempt == 1 {
			// This attempt lost the race and wrote nothing; a concurrent
			// actor closed the bead before the replay reads again. The
			// flip is synchronous here, so this models ordering, not
			// backoff timing.
			markClosed()
			return errors.New(serializationConflictErr)
		}
		t.Errorf("CloseIssue called %d times; the replay must short-circuit on the already-closed bead", attempt)
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Close("gc-1"); err != nil {
		t.Fatalf("Close when another actor closed the bead mid-retry: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 1 {
		t.Fatalf("CloseIssue attempts = %d, want 1 (the replay must not re-close)", got)
	}
	if got := atomic.LoadInt32(&spy.gets); got != 2 {
		t.Fatalf("GetIssue reads = %d, want 2 (the replay must re-read, not reuse the first snapshot)", got)
	}
}

func TestNativeDoltStoreCloseStopsAtAttemptLimit(t *testing.T) {
	spy := newCloseSpy(func(int32, func()) error {
		return errors.New(serializationConflictErr)
	})
	store := newNativeDoltStoreForTest(spy)

	err := store.Close("gc-1")
	if err == nil {
		t.Fatal("Close with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != int32(nativeWriteAttempts) {
		t.Fatalf("CloseIssue attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

func TestNativeDoltStoreCloseDoesNotRetryNonConflictErrors(t *testing.T) {
	spy := newCloseSpy(func(int32, func()) error {
		return errors.New("Error 1062 (23000): duplicate entry")
	})
	store := newNativeDoltStoreForTest(spy)

	err := store.Close("gc-1")
	if err == nil || !strings.Contains(err.Error(), "duplicate entry") {
		t.Fatalf("Close error = %v, want the duplicate-entry error", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 1 {
		t.Fatalf("CloseIssue attempts = %d, want 1 (non-conflict errors must not retry)", got)
	}
}

// TestNativeDoltStoreCloseAllRecoversWhenTheCloseConflictsAfterMetadataLanded
// is the originally reported failure, at the level it was reported. CloseAll
// calls the retried SetMetadataBatch and then Close. While Close was unretried,
// a conflict there returned an error with the metadata already written, leaving
// a bead carrying close metadata that was never closed. That reached users as
// an HTTP 500 on session suspend.
//
// This test drives CloseAll rather than Close so the asymmetry itself is
// guarded. Reverting the Close wrap alone leaves every direct-Close test in
// this file failing, but nothing would record that the pairing is the point.
func TestNativeDoltStoreCloseAllRecoversWhenTheCloseConflictsAfterMetadataLanded(t *testing.T) {
	var (
		closed   int32
		metadata int32
		closes   int32
	)
	issue := func(id string) *beadslib.Issue {
		status := beadslib.StatusOpen
		if atomic.LoadInt32(&closed) == 1 {
			status = beadslib.StatusClosed
		}
		return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2}
	}
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return issue(id), nil
		},
		// CloseAll's own precondition read goes through Get, which uses
		// SearchIssues rather than GetIssue.
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			out := make([]*beadslib.Issue, 0, len(filter.IDs))
			for _, id := range filter.IDs {
				out = append(out, issue(id))
			}
			return out, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			atomic.AddInt32(&metadata, 1)
			return nil
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			if atomic.AddInt32(&closes, 1) == 1 {
				return errors.New(serializationConflictErr)
			}
			atomic.StoreInt32(&closed, 1)
			return nil
		},
	}
	store := newNativeDoltStoreForTest(spy)

	n, err := store.CloseAll([]string{"gc-1"}, map[string]string{"gc.close_reason": "suspended"})
	if err != nil {
		t.Fatalf("CloseAll with one conflicting close: got %v, want nil", err)
	}
	if n != 1 {
		t.Fatalf("CloseAll closed = %d, want 1", n)
	}
	if got := atomic.LoadInt32(&closes); got != 2 {
		t.Fatalf("CloseIssue attempts = %d, want 2 (one conflict, one retry)", got)
	}
	// The metadata write must not be replayed by the Close retry: the two are
	// separately retried units, and only the inner one lost its race.
	if got := atomic.LoadInt32(&metadata); got != 1 {
		t.Fatalf("metadata writes = %d, want 1 (the Close retry must not re-run SetMetadataBatch)", got)
	}
}

// newReopenSpy mirrors newCloseSpy for the opposite transition: the issue
// starts closed and reopenIssue decides what each attempt does.
func newReopenSpy(mutate func(attempt int32, markOpen func()) error) *closeReopenSpy {
	spy := &closeReopenSpy{}
	opened := int32(0)
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			atomic.AddInt32(&spy.gets, 1)
			status := beadslib.StatusClosed
			if atomic.LoadInt32(&opened) == 1 {
				status = beadslib.StatusOpen
			}
			return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2}, nil
		},
		reopenIssue: func(context.Context, string, string, string) error {
			n := atomic.AddInt32(&spy.mutates, 1)
			return mutate(n, func() { atomic.StoreInt32(&opened, 1) })
		},
	}
	return spy
}

func TestNativeDoltStoreReopenRetriesSerializationConflict(t *testing.T) {
	spy := newReopenSpy(func(attempt int32, markOpen func()) error {
		if attempt == 1 {
			return errors.New(serializationConflictErr)
		}
		markOpen()
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Reopen("gc-1"); err != nil {
		t.Fatalf("Reopen after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 2 {
		t.Fatalf("ReopenIssue attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

// Reopen carries the same already-in-target-state short-circuit as Close, so it
// inherits the same replay behavior. Proven separately rather than assumed by
// symmetry: the two guards are different lines against different statuses.
func TestNativeDoltStoreReopenReplayShortCircuitsWhenAnotherActorReopenedTheBead(t *testing.T) {
	spy := newReopenSpy(func(attempt int32, markOpen func()) error {
		if attempt == 1 {
			markOpen()
			return errors.New(serializationConflictErr)
		}
		t.Errorf("ReopenIssue called %d times; the replay must short-circuit on the already-open bead", attempt)
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Reopen("gc-1"); err != nil {
		t.Fatalf("Reopen when another actor reopened the bead mid-retry: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 1 {
		t.Fatalf("ReopenIssue attempts = %d, want 1 (the replay must not re-open)", got)
	}
}
