package beads

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// openIssueForConditionalTest is a minimal well-formed open issue the
// conditional-write spies serve from GetIssue, so nativeCloseReasonFromIssue and
// the row-version read below have real fields to work with.
func openIssueForConditionalTest(id string) *beadslib.Issue {
	return &beadslib.Issue{ID: id, Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}
}

// TestNativeDoltStoreUpdateIfMatchRetriesSerializationConflict guards the
// fenced-write half of the serialization-retry symmetry: UpdateIfMatch must
// absorb a transient conflict exactly like DeleteIfMatch/CloseWithMetadataIfMatch.
// Embedded-Dolt has no internal withRetryTx, so an unwrapped conflict escapes as
// a raw error the nudge-queue CAS loop cannot absorb (it retries only
// PreconditionFailedError) and hard-fails to the API as a 500.
func TestNativeDoltStoreUpdateIfMatchRetriesSerializationConflict(t *testing.T) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return openIssueForConditionalTest(id), nil
		},
		updateIssueChecked: func(_ context.Context, _ string, _ map[string]interface{}, _ string, _ beadslib.UpdateIssueOptions) error {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return errors.New(serializationConflictErr)
			}
			return nil
		},
	}
	store := newNativeDoltStoreForTest(spy)

	status := "in_progress"
	if err := store.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("UpdateIfMatch after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("UpdateIssueChecked attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

func TestNativeDoltStoreUpdateIfMatchStopsAtAttemptLimit(t *testing.T) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return openIssueForConditionalTest(id), nil
		},
		updateIssueChecked: func(_ context.Context, _ string, _ map[string]interface{}, _ string, _ beadslib.UpdateIssueOptions) error {
			atomic.AddInt32(&attempts, 1)
			return errors.New(serializationConflictErr)
		},
	}
	store := newNativeDoltStoreForTest(spy)

	status := "in_progress"
	err := store.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status})
	if err == nil {
		t.Fatal("UpdateIfMatch with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(nativeWriteAttempts) {
		t.Fatalf("UpdateIssueChecked attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

// TestNativeDoltStoreUpdateIfMatchDoesNotRetryVersionMismatch proves the retry
// wrapper leaves the fence itself intact: a version mismatch is ErrVersionMismatch,
// which isNativeDoltSerializationConflict does not match, so the precondition
// failure surfaces on the first attempt rather than being replayed.
func TestNativeDoltStoreUpdateIfMatchDoesNotRetryVersionMismatch(t *testing.T) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return openIssueForConditionalTest(id), nil
		},
		updateIssueChecked: func(_ context.Context, _ string, _ map[string]interface{}, _ string, _ beadslib.UpdateIssueOptions) error {
			atomic.AddInt32(&attempts, 1)
			return beadslib.ErrVersionMismatch
		},
	}
	store := newNativeDoltStoreForTest(spy)

	status := "in_progress"
	err := store.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status})
	if !IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch on version mismatch: got %v, want a precondition failure", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("UpdateIssueChecked attempts = %d, want 1 (a precondition failure must not retry)", got)
	}
}

func TestNativeDoltStoreCloseIfMatchRetriesSerializationConflict(t *testing.T) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return openIssueForConditionalTest(id), nil
		},
		closeIssueChecked: func(_ context.Context, _ string, _ string, _ beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return beadslib.CloseIssueResult{}, errors.New(serializationConflictErr)
			}
			return beadslib.CloseIssueResult{}, nil
		},
	}
	store := newNativeDoltStoreForTest(spy)

	if err := store.CloseIfMatch("gc-1", 7); err != nil {
		t.Fatalf("CloseIfMatch after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("CloseIssueChecked attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

func TestNativeDoltStoreCloseIfMatchStopsAtAttemptLimit(t *testing.T) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return openIssueForConditionalTest(id), nil
		},
		closeIssueChecked: func(_ context.Context, _ string, _ string, _ beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
			atomic.AddInt32(&attempts, 1)
			return beadslib.CloseIssueResult{}, errors.New(serializationConflictErr)
		},
	}
	store := newNativeDoltStoreForTest(spy)

	err := store.CloseIfMatch("gc-1", 7)
	if err == nil {
		t.Fatal("CloseIfMatch with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(nativeWriteAttempts) {
		t.Fatalf("CloseIssueChecked attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

func TestNativeDoltStoreCloseIfMatchDoesNotRetryVersionMismatch(t *testing.T) {
	var attempts int32
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return openIssueForConditionalTest(id), nil
		},
		closeIssueChecked: func(_ context.Context, _ string, _ string, _ beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
			atomic.AddInt32(&attempts, 1)
			return beadslib.CloseIssueResult{}, beadslib.ErrVersionMismatch
		},
	}
	store := newNativeDoltStoreForTest(spy)

	err := store.CloseIfMatch("gc-1", 7)
	if !IsPreconditionFailed(err) {
		t.Fatalf("CloseIfMatch on version mismatch: got %v, want a precondition failure", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("CloseIssueChecked attempts = %d, want 1 (a precondition failure must not retry)", got)
	}
}
