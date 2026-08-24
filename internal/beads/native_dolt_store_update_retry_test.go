package beads

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	mysql "github.com/go-sql-driver/mysql"
	beadslib "github.com/steveyegge/beads"
)

// serializationConflictError models the decoded server response Dolt returns
// when a transaction loses a serialization race. Production replay requires
// the typed rollback proof; matching this text without the MySQL error is
// deliberately insufficient.
func serializationConflictError() error {
	return &mysql.MySQLError{
		Number:  1213,
		Message: "serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction.",
	}
}

// conflictThenOKStorage fails the first failures transactions with a
// serialization conflict, then succeeds, counting every attempt.
func conflictThenOKStorage(failures int32, attempts *int32) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
			if atomic.AddInt32(attempts, 1) <= failures {
				return serializationConflictError()
			}
			return nil
		},
	}
}

func TestNativeDoltStoreUpdateRetriesSerializationConflict(t *testing.T) {
	var attempts int32
	store := newNativeDoltStoreForTest(conflictThenOKStorage(1, &attempts))

	if err := store.Update("gc-1", UpdateOpts{Metadata: map[string]string{"state": "suspended"}}); err != nil {
		t.Fatalf("Update after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("transaction attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

func TestNativeDoltStoreUpdateStopsAtAttemptLimit(t *testing.T) {
	var attempts int32
	store := newNativeDoltStoreForTest(conflictThenOKStorage(int32(nativeWriteAttempts), &attempts))

	err := store.Update("gc-1", UpdateOpts{Metadata: map[string]string{"state": "suspended"}})
	if err == nil {
		t.Fatal("Update with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(nativeWriteAttempts) {
		t.Fatalf("transaction attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

func TestNativeDoltStoreUpdateDoesNotRetryNonConflictErrors(t *testing.T) {
	var attempts int32
	// A constraint violation is a genuine fault: retrying it only multiplies
	// the write load and hides the real error behind a slower failure.
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		runInTransaction: func(context.Context, string, func(beadslib.Transaction) error) error {
			atomic.AddInt32(&attempts, 1)
			return errors.New("Error 1062 (23000): duplicate entry")
		},
	})

	err := store.Update("gc-1", UpdateOpts{Metadata: map[string]string{"state": "suspended"}})
	if err == nil || !strings.Contains(err.Error(), "duplicate entry") {
		t.Fatalf("Update error = %v, want the duplicate-entry error", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("transaction attempts = %d, want 1 (non-conflict errors must not retry)", got)
	}
}
