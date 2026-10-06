package session

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// closeInterferenceStore wraps a store and runs interfere immediately before
// every close attempt, the atomic one and the plain one alike. That is the
// window between Close reading the row and its terminal write. Counting both
// kinds of close proves which arm session.Store.Close took.
type closeInterferenceStore struct {
	beads.Store
	interfere       func(id string) error
	atomicCalls     int
	plainCloseCalls int
	unsupported     bool
}

func (s *closeInterferenceStore) Close(id string) error {
	s.plainCloseCalls++
	if s.interfere != nil {
		if err := s.interfere(id); err != nil {
			return err
		}
	}
	return s.Store.Close(id)
}

func (s *closeInterferenceStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	s.atomicCalls++
	if s.unsupported {
		return beads.Bead{}, fmt.Errorf("atomic close %s: %w", id, beads.ErrConditionalWriteUnsupported)
	}
	if s.interfere != nil {
		if err := s.interfere(id); err != nil {
			return beads.Bead{}, err
		}
	}
	closer, ok := beads.AtomicConditionalCloserFor(s.Store)
	if !ok {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	return closer.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
}

// staleAwakeOnce returns an interference that stamps state=awake exactly once,
// modeling a wake (or a controller holding an older awake observation) that
// lands after Close read the row.
func staleAwakeOnce(store beads.Store) func(string) error {
	fired := false
	return func(id string) error {
		if fired {
			return nil
		}
		fired = true
		return store.SetMetadata(id, "state", string(StateAwake))
	}
}

func seedOpenSession(t *testing.T, store beads.Store, name string) beads.Bead {
	t.Helper()
	created, err := store.Create(sessionBeadFixture(name, "open", map[string]string{"state": string(StateSuspended)}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

var closeTestNow = time.Date(2026, 9, 28, 1, 2, 3, 0, time.UTC)

// TestCloseFencesAStaleAwakeWriterOutOfTheTerminalRow is the regression for
// the closed-but-awake strand. A wake stamps state=awake after Close has read
// the row. Before the fix, Close wrote ClosePatch and then closed the row as
// two writes, so the awake stamp landed between them and the row came to rest
// status=closed state=awake. The reconciler and `gc session list` read that as
// a live session. With a store that provides the atomic close, the stale write
// loses nothing it should keep: the fence refuses the first attempt, Close
// re-reads, and the retry publishes closed and drained in one write.
func TestCloseFencesAStaleAwakeWriterOutOfTheTerminalRow(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-race")
	tracing := &closeInterferenceStore{Store: backing}
	tracing.interfere = staleAwakeOnce(backing)
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(created.ID, string(StateDrained), closeTestNow)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatal("Close reported not-closed for an open session")
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != string(StateDrained) {
		t.Fatalf("terminal row = status %q state %q, want closed/%s", got.Status, got.Metadata["state"], StateDrained)
	}
	if got.Metadata["close_reason"] != CanonicalCloseReason(string(StateDrained)) {
		t.Fatalf("close_reason = %q, want the ClosePatch reason", got.Metadata["close_reason"])
	}
	if tracing.atomicCalls != 2 || tracing.plainCloseCalls != 0 {
		t.Fatalf("atomic/plain close calls = %d/%d, want 2/0 (one lost fence, one retry, no split write)", tracing.atomicCalls, tracing.plainCloseCalls)
	}
}

// TestCloseBoundsRepeatedAtomicRevisionConflicts proves a writer that keeps
// winning the fence cannot livelock Close. It stops after
// terminalCloseMaxAttempts with the precondition error, never falls back to
// the unfenced split write, and leaves the row open for the next pass.
func TestCloseBoundsRepeatedAtomicRevisionConflicts(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-hot")
	tracing := &closeInterferenceStore{Store: backing}
	tracing.interfere = func(id string) error { return backing.SetMetadata(id, "state", string(StateAwake)) }
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(created.ID, string(StateDrained), closeTestNow)
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("Close error = %v, want the bounded precondition failure", err)
	}
	if closed {
		t.Fatal("Close reported closed after every fence was lost")
	}
	if tracing.atomicCalls != terminalCloseMaxAttempts || tracing.plainCloseCalls != 0 {
		t.Fatalf("atomic/plain close calls = %d/%d, want %d/0", tracing.atomicCalls, tracing.plainCloseCalls, terminalCloseMaxAttempts)
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Metadata["close_reason"] != "" {
		t.Fatalf("row after lost fences = status %q close_reason %q, want open with no terminal metadata", got.Status, got.Metadata["close_reason"])
	}
}

// TestCloseReportsFalseWhenAnotherActorClosesFirst covers losing the fence to
// a competing close: the re-read sees the row closed, so Close reports that it
// did not close it (false, nil) and writes nothing more. This is what makes
// session close a claim with exactly one winner.
func TestCloseReportsFalseWhenAnotherActorClosesFirst(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-contended")
	tracing := &closeInterferenceStore{Store: backing}
	fired := false
	tracing.interfere = func(id string) error {
		if fired {
			return nil
		}
		fired = true
		if err := backing.SetMetadata(id, "state", "orphaned"); err != nil {
			return err
		}
		return backing.Close(id)
	}
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(created.ID, string(StateDrained), closeTestNow)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed {
		t.Fatal("Close claimed a close another actor performed")
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != "orphaned" {
		t.Fatalf("row = status %q state %q, want the winner's closed/orphaned untouched", got.Status, got.Metadata["state"])
	}
	if tracing.atomicCalls != 1 {
		t.Fatalf("atomic close calls = %d, want 1 (no retry once the row is closed)", tracing.atomicCalls)
	}
}

// TestConcurrentClosesHaveExactlyOneWinner races real goroutines through the
// front door on an atomic store: exactly one caller reports the close, and
// every caller returns without error.
func TestConcurrentClosesHaveExactlyOneWinner(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-stampede")
	front := NewStore(beads.SessionStore{Store: backing})

	const callers = 8
	results := make([]bool, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var done sync.WaitGroup
	for i := range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			<-start
			results[i], errs[i] = front.Close(created.ID, string(StateDrained), closeTestNow)
		}()
	}
	close(start)
	done.Wait()

	wins := 0
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if results[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("concurrent closes reported %d winners, want exactly 1", wins)
	}
}

// TestCloseFallsBackWhenAtomicCloseIsUnsupportedAtCallTime covers a store that
// advertises the capability but refuses it when called, for example because
// its conditional writes are disabled. Unsupported contractually writes
// nothing, so Close takes the documented two-write fallback instead of failing
// the close.
func TestCloseFallsBackWhenAtomicCloseIsUnsupportedAtCallTime(t *testing.T) {
	backing := beads.NewMemStore()
	created := seedOpenSession(t, backing, "s-degraded")
	tracing := &closeInterferenceStore{Store: backing, unsupported: true}
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(created.ID, string(StateDrained), closeTestNow)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatal("Close reported not-closed on the fallback path")
	}
	if tracing.atomicCalls != 1 || tracing.plainCloseCalls != 1 {
		t.Fatalf("atomic/plain close calls = %d/%d, want 1/1 (refused, then the fallback)", tracing.atomicCalls, tracing.plainCloseCalls)
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != string(StateDrained) {
		t.Fatalf("row = status %q state %q, want closed/%s", got.Status, got.Metadata["state"], StateDrained)
	}
}

// TestCloseSurfacesANonFenceAtomicCloseError proves an atomic-close failure
// that is neither a lost fence nor "unsupported" is returned, not retried and
// not papered over with the split write, which could strand the very state
// the fence exists to prevent.
func TestCloseSurfacesANonFenceAtomicCloseError(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-broken")
	boom := errors.New("storage unavailable")
	tracing := &closeInterferenceStore{Store: backing}
	tracing.interfere = func(string) error { return boom }
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(created.ID, string(StateDrained), closeTestNow)
	if !errors.Is(err, boom) {
		t.Fatalf("Close error = %v, want the store failure", err)
	}
	if closed {
		t.Fatal("Close reported closed after the store failed")
	}
	if tracing.atomicCalls != 1 || tracing.plainCloseCalls != 0 {
		t.Fatalf("atomic/plain close calls = %d/%d, want 1/0", tracing.atomicCalls, tracing.plainCloseCalls)
	}
}
