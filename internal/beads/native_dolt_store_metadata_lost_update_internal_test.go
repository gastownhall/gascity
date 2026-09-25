package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// lostUpdateStorage models the two backend properties the metadata write path
// depends on: an unchecked UpdateIssue replaces the row it names whatever
// happened since the caller's read, and UpdateIssueChecked refuses with
// ErrVersionMismatch when the row's version no longer equals the one the
// caller expects. A concurrent update lands exactly once, right after the
// first read the store performs, which is the interleaving observed in the
// audit trail (the fence activation committing between a metadata read and
// its write-back).
type lostUpdateStorage struct {
	beadslib.Storage
	durable         *beadslib.Issue
	concurrent      map[string]string
	fired           bool
	reads           int
	uncheckedWrites int
	// checkedWrites records the ExpectedVersion each checked write carried.
	checkedWrites []int64
}

func (s *lostUpdateStorage) fireConcurrentUpdate() {
	if s.fired {
		return
	}
	s.fired = true
	s.durable.Metadata = mergeNativeMetadataForTest(s.durable.Metadata, s.concurrent)
	s.durable.RowVersion++
}

func (s *lostUpdateStorage) GetIssue(context.Context, string) (*beadslib.Issue, error) {
	s.reads++
	snapshot := cloneNativeIssueForTest(s.durable)
	s.fireConcurrentUpdate()
	return snapshot, nil
}

func (s *lostUpdateStorage) replaceMetadata(updates map[string]interface{}) error {
	raw, ok := updates["metadata"].(json.RawMessage)
	if !ok {
		return errors.New("metadata update is not json.RawMessage")
	}
	s.durable.Metadata = append(json.RawMessage(nil), raw...)
	s.durable.RowVersion++
	return nil
}

func (s *lostUpdateStorage) UpdateIssue(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
	s.uncheckedWrites++
	return s.replaceMetadata(updates)
}

func (s *lostUpdateStorage) UpdateIssueChecked(_ context.Context, _ string, updates map[string]interface{}, _ string, opts beadslib.UpdateIssueOptions) error {
	if opts.ExpectedVersion == nil {
		return errors.New("checked write carried no expected version")
	}
	s.checkedWrites = append(s.checkedWrites, *opts.ExpectedVersion)
	if *opts.ExpectedVersion != s.durable.RowVersion {
		return fmt.Errorf("%w: expected %d, got %d", beadslib.ErrVersionMismatch, *opts.ExpectedVersion, s.durable.RowVersion)
	}
	return s.replaceMetadata(updates)
}

func mergeNativeMetadataForTest(raw json.RawMessage, kvs map[string]string) json.RawMessage {
	metadata := map[string]string{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			panic(err)
		}
	}
	for k, v := range kvs {
		metadata[k] = v
	}
	out, err := json.Marshal(metadata)
	if err != nil {
		panic(err)
	}
	return out
}

// TestNativeDoltStoreMetadataWriteKeepsAConcurrentUpdate pins that a metadata
// write never undoes an update that committed between its read and its
// write. The store merges the new keys into the map it read and writes the
// whole map back, so an unchecked write-back replaces every key the
// concurrent update touched with the values from before it. The interleaving
// here is the audit trail of a lost review workflow: a graph step's fence
// activation (routed_to restored, fence keys cleared) committed between a
// heartbeat-shaped stamp's read and its write, and the stamp put the fence
// back.
func TestNativeDoltStoreMetadataWriteKeepsAConcurrentUpdate(t *testing.T) {
	const (
		id          = "gc-fenced-step"
		readVersion = int64(7)
	)
	activation := map[string]string{
		"gc.instantiating":      "",
		"gc.deferred_routed_to": "",
		"gc.routed_to":          "rig/pool",
	}
	newStorage := func() *lostUpdateStorage {
		return &lostUpdateStorage{
			durable: &beadslib.Issue{
				ID:         id,
				Title:      "Resolve PR identity",
				Status:     beadslib.StatusOpen,
				IssueType:  beadslib.TypeTask,
				Priority:   2,
				Metadata:   json.RawMessage(`{"gc.run_target":"pool","gc.instantiating":"true","gc.deferred_routed_to":"rig/pool"}`),
				RowVersion: readVersion,
			},
			concurrent: activation,
		}
	}
	assertKept := func(t *testing.T, storage *lostUpdateStorage, key, want string) {
		t.Helper()
		metadata, err := metadataMapFromNative(storage.durable.Metadata)
		if err != nil {
			t.Fatalf("parse durable metadata: %v", err)
		}
		if got := metadata[key]; got != want {
			t.Fatalf("%s = %q after the metadata write, want %q (the concurrent update was overwritten by the stale map)", key, got, want)
		}
	}
	assertRetriedFromAFreshRead := func(t *testing.T, storage *lostUpdateStorage) {
		t.Helper()
		if !storage.fired {
			t.Fatal("the concurrent update never landed; the interleaving was not exercised")
		}
		if storage.uncheckedWrites != 0 {
			t.Fatalf("unchecked writes = %d, want 0: an unchecked write-back cannot see the concurrent update", storage.uncheckedWrites)
		}
		if want := []int64{readVersion, readVersion + 1}; !slicesEqualInt64(storage.checkedWrites, want) {
			t.Fatalf("checked writes carried versions %v, want %v: the refused swap must be retried from a fresh read", storage.checkedWrites, want)
		}
		if storage.reads != 2 {
			t.Fatalf("reads = %d, want 2 (the first read, then the re-read after the refused swap)", storage.reads)
		}
	}

	t.Run("SetMetadataBatch", func(t *testing.T) {
		storage := newStorage()
		store := newNativeDoltStoreForTest(storage)
		if err := store.SetMetadataBatch(id, map[string]string{"gc.heartbeat": "now"}); err != nil {
			t.Fatalf("SetMetadataBatch: %v", err)
		}
		assertKept(t, storage, "gc.instantiating", "")
		assertKept(t, storage, "gc.deferred_routed_to", "")
		assertKept(t, storage, "gc.routed_to", "rig/pool")
		assertKept(t, storage, "gc.heartbeat", "now")
		assertRetriedFromAFreshRead(t, storage)
	})

	t.Run("SetMetadata", func(t *testing.T) {
		storage := newStorage()
		store := newNativeDoltStoreForTest(storage)
		if err := store.SetMetadata(id, "gc.heartbeat", "now"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		assertKept(t, storage, "gc.instantiating", "")
		assertKept(t, storage, "gc.routed_to", "rig/pool")
		assertKept(t, storage, "gc.heartbeat", "now")
		assertRetriedFromAFreshRead(t, storage)
	})
}

// TestNativeDoltStoreMetadataWriteGivesUpAfterRepeatedVersionMismatches pins
// the retry budget: a swap refused on every attempt is returned to the caller
// after nativeWriteAttempts fresh reads, not retried without bound.
func TestNativeDoltStoreMetadataWriteGivesUpAfterRepeatedVersionMismatches(t *testing.T) {
	getCalls := 0
	checkedCalls := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			getCalls++
			return &beadslib.Issue{ID: "gc-contended", RowVersion: int64(getCalls)}, nil
		},
		updateIssueChecked: func(_ context.Context, _ string, _ map[string]interface{}, _ string, opts beadslib.UpdateIssueOptions) error {
			checkedCalls++
			return fmt.Errorf("%w: expected %d, got %d", beadslib.ErrVersionMismatch, *opts.ExpectedVersion, *opts.ExpectedVersion+1)
		},
	}
	store := newNativeDoltStoreForTest(storage)

	err := store.SetMetadataBatch("gc-contended", map[string]string{"requested": "written"})
	if !errors.Is(err, beadslib.ErrVersionMismatch) {
		t.Fatalf("SetMetadataBatch error = %v, want ErrVersionMismatch after the retry budget", err)
	}
	// Exhausted contention must read as the transient CAS-exhaustion class, so
	// callers that classify errors re-enter rather than fail the work.
	var exhausted *CASRetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("SetMetadataBatch error = %v, want it to wrap *CASRetriesExhaustedError", err)
	}
	// The spy's reads return versions 1..nativeWriteAttempts, so the last
	// refused swap expected nativeWriteAttempts.
	if exhausted.ID != "gc-contended" || exhausted.Key != "requested" || exhausted.Attempts != nativeWriteAttempts ||
		exhausted.LastRevision != int64(nativeWriteAttempts) {
		t.Fatalf("CASRetriesExhaustedError = %+v, want ID gc-contended, Key requested, Attempts %d, LastRevision %d",
			*exhausted, nativeWriteAttempts, nativeWriteAttempts)
	}
	if getCalls != nativeWriteAttempts || checkedCalls != nativeWriteAttempts {
		t.Fatalf("calls = GetIssue:%d UpdateIssueChecked:%d, want %d each", getCalls, checkedCalls, nativeWriteAttempts)
	}
}

func slicesEqualInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNativeDoltStoreMetadataWriteDoesNotMarkOtherFailuresAsExhausted pins
// that only a version mismatch that outlived the budget is reported as CAS
// exhaustion: a serialization conflict that outlives it, or a permanent error,
// is returned as it was.
func TestNativeDoltStoreMetadataWriteDoesNotMarkOtherFailuresAsExhausted(t *testing.T) {
	for name, writeErr := range map[string]error{
		"serialization conflict": errors.New("Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction"),
		"permanent":              errors.New("disk full"),
	} {
		t.Run(name, func(t *testing.T) {
			storage := &nativeDoltStorageSpy{
				getIssue: func(context.Context, string) (*beadslib.Issue, error) {
					return &beadslib.Issue{ID: "gc-x"}, nil
				},
				updateIssueChecked: func(context.Context, string, map[string]interface{}, string, beadslib.UpdateIssueOptions) error {
					return writeErr
				},
			}
			err := newNativeDoltStoreForTest(storage).SetMetadataBatch("gc-x", map[string]string{"k": "v"})
			if err == nil {
				t.Fatal("SetMetadataBatch succeeded, want the write error")
			}
			if IsCASRetriesExhausted(err) {
				t.Fatalf("SetMetadataBatch error = %v, want no CAS-exhaustion wrapping for a non-mismatch failure", err)
			}
		})
	}
}
