package beads

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// openIssueForConditionalTest is a minimal well-formed open issue the
// conditional-write doubles serve, so nativeCloseReasonFromIssue and the
// row-version read below have real fields to work with.
func openIssueForConditionalTest() *beadslib.Issue {
	return &beadslib.Issue{ID: "gc-1", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}
}

// retryCountingLifecycle is a minimal issueops.Lifecycle whose Update and
// Close calls are test-supplied, for pinning the serialization-retry wrapper
// UpdateIfMatch/CloseIfMatch install around the role door -- without a real
// backend and without the raw updateIssueChecked/closeIssueChecked hooks the
// role-based rewrite no longer calls.
type retryCountingLifecycle struct {
	update func(issueops.UpdateRequest) (issueops.UpdateResult, error)
	close  func(issueops.CloseRequest) (issueops.CloseResult, error)
}

var _ issueops.Lifecycle = retryCountingLifecycle{}

func (l retryCountingLifecycle) Create(context.Context, issueops.CreateRequest) (issueops.CreateResult, error) {
	panic("retryCountingLifecycle: Create not exercised by this test")
}

func (l retryCountingLifecycle) Update(_ context.Context, req issueops.UpdateRequest) (issueops.UpdateResult, error) {
	return l.update(req)
}

func (l retryCountingLifecycle) Close(_ context.Context, req issueops.CloseRequest) (issueops.CloseResult, error) {
	return l.close(req)
}

func (l retryCountingLifecycle) Reopen(context.Context, issueops.ReopenRequest) (issueops.ReopenResult, error) {
	panic("retryCountingLifecycle: Reopen not exercised by this test")
}

// newRetryCountingStore wires a nativeDoltStorageSpy whose IssueLifecycle is
// the given fake and whose raw GetIssue (which nativeDoltStorageSpy's
// IssueReader() wraps as issueops.Reader.Get, per
// native_dolt_store_reader_doubles_test.go) serves one fixed, never-mutated
// issue -- CloseIfMatch pre-reads it for the close reason before ever
// touching the lifecycle role.
func newRetryCountingStore(t *testing.T, lifecycle issueops.Lifecycle) *NativeDoltStore {
	t.Helper()
	fixture := openIssueForConditionalTest()
	spy := &nativeDoltStorageSpy{
		issueLifecycle: func() (issueops.Lifecycle, error) { return lifecycle, nil },
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			if id != fixture.ID {
				return nil, nil
			}
			return cloneNativeIssueForTest(fixture), nil
		},
	}
	return newNativeDoltStoreForTest(spy)
}

// TestNativeDoltStoreUpdateIfMatchRetriesSerializationConflict guards the
// fenced-write half of the serialization-retry symmetry: UpdateIfMatch must
// absorb a transient conflict exactly like DeleteIfMatch/CloseWithMetadataIfMatch.
// Embedded-Dolt has no internal withRetryTx, so an unwrapped conflict escapes as
// a raw error the nudge-queue CAS loop cannot absorb (it retries only
// PreconditionFailedError) and hard-fails to the API as a 500.
func TestNativeDoltStoreUpdateIfMatchRetriesSerializationConflict(t *testing.T) {
	var attempts int32
	lifecycle := retryCountingLifecycle{
		update: func(issueops.UpdateRequest) (issueops.UpdateResult, error) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return issueops.UpdateResult{}, errors.New(serializationConflictErr)
			}
			return issueops.UpdateResult{Changed: true}, nil
		},
	}
	store := newRetryCountingStore(t, lifecycle)

	status := "in_progress"
	if err := store.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("UpdateIfMatch after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("Lifecycle.Update attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

func TestNativeDoltStoreUpdateIfMatchStopsAtAttemptLimit(t *testing.T) {
	var attempts int32
	lifecycle := retryCountingLifecycle{
		update: func(issueops.UpdateRequest) (issueops.UpdateResult, error) {
			atomic.AddInt32(&attempts, 1)
			return issueops.UpdateResult{}, errors.New(serializationConflictErr)
		},
	}
	store := newRetryCountingStore(t, lifecycle)

	status := "in_progress"
	err := store.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status})
	if err == nil {
		t.Fatal("UpdateIfMatch with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(nativeWriteAttempts) {
		t.Fatalf("Lifecycle.Update attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

// TestNativeDoltStoreUpdateIfMatchDoesNotRetryVersionMismatch proves the retry
// wrapper leaves the fence itself intact: a version mismatch is
// issueops.ErrVersionMismatch, which isNativeDoltSerializationConflict does
// not match, so the precondition failure surfaces on the first attempt rather
// than being replayed.
func TestNativeDoltStoreUpdateIfMatchDoesNotRetryVersionMismatch(t *testing.T) {
	var attempts int32
	lifecycle := retryCountingLifecycle{
		update: func(issueops.UpdateRequest) (issueops.UpdateResult, error) {
			atomic.AddInt32(&attempts, 1)
			return issueops.UpdateResult{}, issueops.ErrVersionMismatch
		},
	}
	store := newRetryCountingStore(t, lifecycle)

	status := "in_progress"
	err := store.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status})
	if !IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch on version mismatch: got %v, want a precondition failure", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("Lifecycle.Update attempts = %d, want 1 (a precondition failure must not retry)", got)
	}
}

func TestNativeDoltStoreCloseIfMatchRetriesSerializationConflict(t *testing.T) {
	var attempts int32
	lifecycle := retryCountingLifecycle{
		close: func(issueops.CloseRequest) (issueops.CloseResult, error) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return issueops.CloseResult{}, errors.New(serializationConflictErr)
			}
			return issueops.CloseResult{Changed: true}, nil
		},
	}
	store := newRetryCountingStore(t, lifecycle)

	if err := store.CloseIfMatch("gc-1", 7); err != nil {
		t.Fatalf("CloseIfMatch after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("Lifecycle.Close attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

func TestNativeDoltStoreCloseIfMatchStopsAtAttemptLimit(t *testing.T) {
	var attempts int32
	lifecycle := retryCountingLifecycle{
		close: func(issueops.CloseRequest) (issueops.CloseResult, error) {
			atomic.AddInt32(&attempts, 1)
			return issueops.CloseResult{}, errors.New(serializationConflictErr)
		},
	}
	store := newRetryCountingStore(t, lifecycle)

	err := store.CloseIfMatch("gc-1", 7)
	if err == nil {
		t.Fatal("CloseIfMatch with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(nativeWriteAttempts) {
		t.Fatalf("Lifecycle.Close attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

func TestNativeDoltStoreCloseIfMatchDoesNotRetryVersionMismatch(t *testing.T) {
	var attempts int32
	lifecycle := retryCountingLifecycle{
		close: func(issueops.CloseRequest) (issueops.CloseResult, error) {
			atomic.AddInt32(&attempts, 1)
			return issueops.CloseResult{}, issueops.ErrVersionMismatch
		},
	}
	store := newRetryCountingStore(t, lifecycle)

	err := store.CloseIfMatch("gc-1", 7)
	if !IsPreconditionFailed(err) {
		t.Fatalf("CloseIfMatch on version mismatch: got %v, want a precondition failure", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("Lifecycle.Close attempts = %d, want 1 (a precondition failure must not retry)", got)
	}
}

// retryInjectingStorage wraps a real, stateful nativeDoltRawPanicStorage
// fixture (the same one native_dolt_store_conditional_raw_panic_test.go uses
// to prove no raw primitive is reached) and forces the first len(batchErrors)
// ApplyBatch calls, or the first len(deleteErrors) Delete calls, to fail
// WITHOUT touching the fixture, before falling through to the real,
// version-fenced logic. This pins retryOnNativeDoltSerializationConflict's
// replay behavior for CloseWithMetadataIfMatch/DeleteIfMatch against a fixture
// that can actually detect a stale fence on retry, which a bare attempt
// counter cannot.
type retryInjectingStorage struct {
	*nativeDoltRawPanicStorage
	batchErrors   []error
	batchCalls    int
	deleteErrors  []error
	deleteCalls   int
	afterConflict func()
}

func newRetryInjectingStorage(issue *beadslib.Issue) *retryInjectingStorage {
	return &retryInjectingStorage{nativeDoltRawPanicStorage: newNativeDoltRawPanicStorage(issue)}
}

func (s *retryInjectingStorage) BatchApplier() (issueops.BatchApplier, error) {
	return retryInjectingBatchApplier{storage: s}, nil
}

type retryInjectingBatchApplier struct{ storage *retryInjectingStorage }

var _ issueops.BatchApplier = retryInjectingBatchApplier{}

func (a retryInjectingBatchApplier) ApplyBatch(ctx context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	s := a.storage
	s.batchCalls++
	if len(s.batchErrors) > 0 {
		err := s.batchErrors[0]
		s.batchErrors = s.batchErrors[1:]
		if s.afterConflict != nil {
			s.afterConflict()
		}
		return issueops.ApplyBatchResult{}, err
	}
	return rawPanicBatchApplier{storage: s.nativeDoltRawPanicStorage}.ApplyBatch(ctx, req)
}

func (s *retryInjectingStorage) Deleter() (issueops.Deleter, error) {
	return retryInjectingDeleter{storage: s}, nil
}

type retryInjectingDeleter struct{ storage *retryInjectingStorage }

var _ issueops.Deleter = retryInjectingDeleter{}

func (d retryInjectingDeleter) Delete(ctx context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	s := d.storage
	s.deleteCalls++
	if len(s.deleteErrors) > 0 {
		err := s.deleteErrors[0]
		s.deleteErrors = s.deleteErrors[1:]
		if s.afterConflict != nil {
			s.afterConflict()
		}
		return issueops.DeleteResult{}, err
	}
	return rawPanicDeleter{storage: s.nativeDoltRawPanicStorage}.Delete(ctx, req)
}

// nativeDoltConditionalRetryConflicts are the two Dolt/MySQL error shapes
// isNativeDoltSerializationConflict classifies as transient and retryable.
func nativeDoltConditionalRetryConflicts() []error {
	return []error{
		errors.New("Error 1213 (40001): deadlock"),
		errors.New("Error 1205 (HY000): lock wait timeout exceeded"),
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchRetriesWholeApplyBatchCall(t *testing.T) {
	for _, conflict := range nativeDoltConditionalRetryConflicts() {
		t.Run(conflict.Error(), func(t *testing.T) {
			storage := newRetryInjectingStorage(openIssueForConditionalTest())
			storage.batchErrors = []error{conflict}
			store := newNativeDoltStoreForTest(storage)

			closed, err := store.CloseWithMetadataIfMatch("gc-1", 0, map[string]string{"state": "drained"})
			if err != nil {
				t.Fatalf("CloseWithMetadataIfMatch: %v", err)
			}
			if storage.batchCalls != 2 {
				t.Fatalf("ApplyBatch calls = %d, want 2", storage.batchCalls)
			}
			if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
				t.Fatalf("returned bead = %#v, want closed row from replay", closed)
			}
		})
	}
}

// TestNativeDoltStoreCloseWithMetadataIfMatchRetryRereadsFence pins that a
// retry replays the ExpectedVersion fence against the row AS IT STANDS AT
// RETRY TIME, not the (now stale) version the failed first attempt saw: an
// intervening write between attempts must turn the retry into a precondition
// failure rather than silently clobbering it.
func TestNativeDoltStoreCloseWithMetadataIfMatchRetryRereadsFence(t *testing.T) {
	storage := newRetryInjectingStorage(openIssueForConditionalTest())
	storage.batchErrors = []error{errors.New("Error 1213 (40001): deadlock")}
	storage.afterConflict = func() {
		raw, err := metadataRawFromMap(map[string]string{"intervening": "write"})
		if err != nil {
			t.Fatalf("intervening metadata: %v", err)
		}
		storage.issue.Metadata = raw
		storage.issue.RowVersion++
	}
	store := newNativeDoltStoreForTest(storage)

	closed, err := store.CloseWithMetadataIfMatch("gc-1", 0, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want precondition failure", err)
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("failed replay returned %#v, want zero bead", closed)
	}
	if storage.batchCalls != 2 {
		t.Fatalf("ApplyBatch calls = %d, want 2", storage.batchCalls)
	}
	fresh, err := store.Get("gc-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fresh.Status != "open" || fresh.Metadata["state"] != "" || fresh.Metadata["intervening"] != "write" {
		t.Fatalf("replay fence result = %#v, want later open row without close", fresh)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchDoesNotRetryAmbiguousFailure(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	storage := newRetryInjectingStorage(openIssueForConditionalTest())
	storage.batchErrors = []error{sentinel}
	store := newNativeDoltStoreForTest(storage)

	closed, err := store.CloseWithMetadataIfMatch("gc-1", 0, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want %v", err, sentinel)
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("ambiguous failure returned %#v, want zero bead", closed)
	}
	if storage.batchCalls != 1 {
		t.Fatalf("ApplyBatch calls = %d, want 1", storage.batchCalls)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchReturnsZeroAfterRetryExhaustion(t *testing.T) {
	conflict := errors.New("Error 1213 (40001): deadlock")
	storage := newRetryInjectingStorage(openIssueForConditionalTest())
	storage.batchErrors = []error{conflict, conflict, conflict}
	store := newNativeDoltStoreForTest(storage)

	closed, err := store.CloseWithMetadataIfMatch("gc-1", 0, nil)
	if !errors.Is(err, conflict) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want %v", err, conflict)
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("exhausted retry returned %#v, want zero bead", closed)
	}
	if storage.batchCalls != nativeWriteAttempts {
		t.Fatalf("ApplyBatch calls = %d, want %d", storage.batchCalls, nativeWriteAttempts)
	}
}

// DeleteIfMatch's fence check and delete are one Deleter.Delete call, so a
// serialization conflict must replay the WHOLE call -- re-reading the row
// version each attempt -- exactly like the close path. Retrying some smaller
// slice would fence against a RowVersion an earlier, failed attempt never
// actually invalidated.
func TestNativeDoltStoreDeleteIfMatchRetriesWholeDeleteCall(t *testing.T) {
	for _, conflict := range nativeDoltConditionalRetryConflicts() {
		t.Run(conflict.Error(), func(t *testing.T) {
			storage := newRetryInjectingStorage(openIssueForConditionalTest())
			storage.deleteErrors = []error{conflict}
			store := newNativeDoltStoreForTest(storage)

			if err := store.DeleteIfMatch("gc-1", 0); err != nil {
				t.Fatalf("DeleteIfMatch: %v", err)
			}
			if storage.deleteCalls != 2 {
				t.Fatalf("Delete calls = %d, want 2", storage.deleteCalls)
			}
			if _, err := store.Get("gc-1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after replayed delete = %v, want ErrNotFound", err)
			}
		})
	}
}

// An ambiguous failure -- one that may have committed -- must NOT replay a
// delete, or a delete that already applied would run again against a moved
// fence. This pins the same transient/ambiguous split the close path draws.
func TestNativeDoltStoreDeleteIfMatchDoesNotRetryAmbiguousFailure(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	storage := newRetryInjectingStorage(openIssueForConditionalTest())
	storage.deleteErrors = []error{sentinel}
	store := newNativeDoltStoreForTest(storage)

	if err := store.DeleteIfMatch("gc-1", 0); !errors.Is(err, sentinel) {
		t.Fatalf("DeleteIfMatch error = %v, want %v", err, sentinel)
	}
	if storage.deleteCalls != 1 {
		t.Fatalf("Delete calls = %d, want 1", storage.deleteCalls)
	}
}
