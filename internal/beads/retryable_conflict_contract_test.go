package beads

import (
	"errors"
	"fmt"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

func TestIsRetryableConflictContract(t *testing.T) {
	cas := &CASRetriesExhaustedError{ID: "ga-session", Key: "state", Attempts: 3}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"Dolt 1213", errors.New("Error 1213: deadlock"), true},
		{"Dolt 1213 lowercase", errors.New("error 1213: deadlock"), true},
		{"Dolt 1205", errors.New("Error 1205: lock wait timeout"), true},
		{"SQLSTATE 40001", errors.New("commit failed: SQLSTATE 40001"), true},
		{"parenthesized SQLSTATE", errors.New("commit failed (40001)"), true},
		{"Dolt wording", errors.New("this transaction conflicts with a committed transaction"), true},
		{"CAS exhaustion", cas, true},
		{"bead ID contains digits", errors.New("read ga-40001x failed"), false},
		{"not found", ErrNotFound, false},
		{"nil", nil, false},
		{"bare version mismatch", beadslib.ErrVersionMismatch, false},
		{"MySQL timeout", errors.New("[mysql] read tcp: i/o timeout"), false},
		{"invalid connection", errors.New("invalid connection"), false},
		{"indeterminate commit", errors.New("write commit result indeterminate after connection loss (not retried to avoid double-apply)"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryableConflict(tt.err); got != tt.want {
				t.Fatalf("IsRetryableConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
			if tt.want {
				wrapped := fmt.Errorf("update: %w", tt.err)
				if !IsRetryableConflict(wrapped) {
					t.Fatalf("IsRetryableConflict(%v) = false, want true for wrapped conflict", wrapped)
				}
			}
		})
	}
}

func TestRetryNativeDoltWriteBoundAndRecovery(t *testing.T) {
	conflict := errors.New("Error 1213: serialization conflict")
	permanent := errors.New("permission denied")
	for _, tt := range []struct {
		name, stop string
		sequence   []error
		wantCalls  int
		wantErr    error
	}{
		{"recovery on second", "success", []error{conflict, nil}, 2, nil},
		{"recovery on third", "success", []error{conflict, conflict, nil}, 3, nil},
		{"exhaustion", "budget", []error{conflict, conflict, conflict}, 3, conflict},
		{"permanent", "immediate", []error{permanent, nil}, 1, permanent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			start := time.Now()
			err := retryNativeDoltWrite(func() error {
				defer func() { calls++ }()
				return tt.sequence[calls]
			}, isNativeDoltSerializationConflict)
			elapsed := time.Since(start)
			if calls != tt.wantCalls || !errors.Is(err, tt.wantErr) {
				t.Fatalf("retry = (%d calls, %v), want (%d calls, %v)", calls, err, tt.wantCalls, tt.wantErr)
			}
			if tt.stop == "budget" && (elapsed < 75*time.Millisecond || elapsed > 2*time.Second) {
				t.Fatalf("exhausted retry took %s, want 75ms backoff with generous 2s ceiling", elapsed)
			}
		})
	}
}

func TestNativeDoltStoreMetadataConflictThenVersionMismatchStops(t *testing.T) {
	writes := 0
	store := newRetryCountingStore(t, retryCountingLifecycle{
		update: func(issueops.UpdateRequest) (issueops.UpdateResult, error) {
			writes++
			if writes == 1 {
				return issueops.UpdateResult{}, errors.New("Error 1213 (40001): serialization conflict")
			}
			return issueops.UpdateResult{}, beadslib.ErrVersionMismatch
		},
	})
	err := store.SetMetadataBatch("gc-1", map[string]string{"state": "active"})
	if writes != 2 || !errors.Is(err, beadslib.ErrVersionMismatch) || IsCASRetriesExhausted(err) {
		t.Fatalf("mixed conflict = (%d writes, %v), want one serialization retry then the version mismatch without CAS exhaustion", writes, err)
	}
}
