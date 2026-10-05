package session

import (
	"errors"
	"fmt"
	"strconv"
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

// unrelatedKey is a metadata key the lifecycle projection does not read.
const unrelatedKey = "last_nudge_delivered_at"

// unrelatedWriteOnce returns an interference that writes unrelatedKey exactly
// once. It moves the revision without touching any fact a close is decided
// on, so a fenced close must retry and still close.
func unrelatedWriteOnce(store beads.Store) func(string) error {
	fired := false
	return func(id string) error {
		if fired {
			return nil
		}
		fired = true
		return store.SetMetadata(id, unrelatedKey, "2026-09-28T01:02:03Z")
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

// decidedOn reads id from store and returns the Info a caller would decide a
// close on: the row as it is right now.
func decidedOn(t *testing.T, store beads.Store, id string) Info {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return infoFromPersistedBead(b)
}

// TestCloseFencesAStaleAwakeWriterOutOfTheTerminalRow is the regression for
// the closed-but-awake strand and for closing over a wake. A wake stamps
// state=awake after Close has read the row. Before the atomic close, Close
// wrote ClosePatch and then closed the row as two writes, so the awake stamp
// landed between them and the row came to rest status=closed state=awake.
// With the atomic close, the fence refused the first attempt, but the retry
// re-read only the revision and closed the woken session anyway. The wake
// changed the facts the close was decided on, so Close must now write nothing
// and return ErrSessionCloseSuperseded, leaving the woken row open for the
// next pass to decide.
func TestCloseFencesAStaleAwakeWriterOutOfTheTerminalRow(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-race")
	tracing := &closeInterferenceStore{Store: backing}
	tracing.interfere = staleAwakeOnce(backing)
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
	if !errors.Is(err, ErrSessionCloseSuperseded) || closed {
		t.Fatalf("Close = (%v, %v), want (false, ErrSessionCloseSuperseded) after a wake won the fence", closed, err)
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Metadata["state"] != string(StateAwake) || got.Metadata["close_reason"] != "" {
		t.Fatalf("row = status %q state %q close_reason %q, want the wake's open/awake row untouched", got.Status, got.Metadata["state"], got.Metadata["close_reason"])
	}
	if tracing.atomicCalls != 1 || tracing.plainCloseCalls != 0 {
		t.Fatalf("atomic/plain close calls = %d/%d, want 1/0 (one lost fence, no retry, no split write)", tracing.atomicCalls, tracing.plainCloseCalls)
	}
}

// TestCloseRefusesAWakeBetweenTheDecisionAndTheFirstRead is the regression
// for a close premise taken from the close's own first read. The caller decides
// to close a suspended row, and a wake stamps state=awake before the close
// reads it. That read then agreed with itself, so the close landed on the woken
// row. Close and CloseWithTerminalPatch now check the caller's Info on every
// read, the first included, and write nothing. Close's fallback arm reads the
// row too, so a store without the atomic close refuses as well.
func TestCloseRefusesAWakeBetweenTheDecisionAndTheFirstRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backing func() beads.Store
		// plain hands the front door the bare store, so a store without the
		// atomic close takes the two-write fallback arm.
		plain bool
		close func(front *Store, decided Info) (bool, error)
	}{
		{
			name:    "Close/atomic",
			backing: beads.NewAtomicCloseMemStore,
			close: func(front *Store, decided Info) (bool, error) {
				return front.Close(decided, string(StateDrained), closeTestNow)
			},
		},
		{
			name:    "Close/fallback",
			backing: func() beads.Store { return beads.NewMemStore() },
			plain:   true,
			close: func(front *Store, decided Info) (bool, error) {
				return front.Close(decided, string(StateDrained), closeTestNow)
			},
		},
		{
			name:    "CloseWithTerminalPatch/atomic",
			backing: beads.NewAtomicCloseMemStore,
			close: func(front *Store, decided Info) (bool, error) {
				return front.CloseWithTerminalPatch(decided, ClosePatch(closeTestNow, "dead-runtime"), "gc: close session", closeTestNow)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := tc.backing()
			created := seedOpenSession(t, backing, "s-woken")
			decided := decidedOn(t, backing, created.ID)
			if err := backing.SetMetadata(created.ID, "state", string(StateAwake)); err != nil {
				t.Fatalf("stamping the wake: %v", err)
			}
			tracing := &closeInterferenceStore{Store: backing}
			var frontStore beads.Store = tracing
			if tc.plain {
				frontStore = backing
			}

			closed, err := tc.close(NewStore(beads.SessionStore{Store: frontStore}), decided)
			if !errors.Is(err, ErrSessionCloseSuperseded) || closed {
				t.Fatalf("close = (%v, %v), want (false, ErrSessionCloseSuperseded) for a row woken after the decision", closed, err)
			}
			got, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "open" || got.Metadata["state"] != string(StateAwake) || got.Metadata["close_reason"] != "" {
				t.Fatalf("row = status %q state %q close_reason %q, want the woken row untouched", got.Status, got.Metadata["state"], got.Metadata["close_reason"])
			}
			if tracing.atomicCalls != 0 || tracing.plainCloseCalls != 0 {
				t.Fatalf("atomic/plain close calls = %d/%d, want 0/0 (refused on the first read)", tracing.atomicCalls, tracing.plainCloseCalls)
			}
		})
	}
}

// TestCloseRetriesPastAWriteOutsideItsPremise proves the retry still serves
// its purpose: a writer that moves the revision without touching a lifecycle
// fact (a nudge stamp) wins the fence once, and the retry closes the row in
// one write, keeping that writer's key.
func TestCloseRetriesPastAWriteOutsideItsPremise(t *testing.T) {
	backing := beads.NewAtomicCloseMemStore()
	created := seedOpenSession(t, backing, "s-unrelated")
	tracing := &closeInterferenceStore{Store: backing}
	tracing.interfere = unrelatedWriteOnce(backing)
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
	if err != nil || !closed {
		t.Fatalf("Close = (%v, %v), want (true, nil)", closed, err)
	}
	got, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" || got.Metadata["state"] != string(StateDrained) || got.Metadata[unrelatedKey] == "" {
		t.Fatalf("row = status %q state %q %s %q, want closed/%s with the unrelated write kept", got.Status, got.Metadata["state"], unrelatedKey, got.Metadata[unrelatedKey], StateDrained)
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
	writes := 0
	tracing.interfere = func(id string) error {
		writes++
		return backing.SetMetadata(id, unrelatedKey, strconv.Itoa(writes))
	}
	front := NewStore(beads.SessionStore{Store: tracing})

	closed, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
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

	closed, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
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
	decided := decidedOn(t, backing, created.ID)

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
			results[i], errs[i] = front.Close(decided, string(StateDrained), closeTestNow)
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

	closed, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
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

	closed, err := front.Close(decidedOn(t, backing, created.ID), string(StateDrained), closeTestNow)
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
