package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout/gate"
	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypes protects the
// native metadata boundary that the string-valued Store projection otherwise
// hides. A value-CAS changes exactly one logical string key; boolean, number,
// null, object, array, and string siblings must retain their JSON types.
func TestNativeDoltStoreMetadataCASPreservesMixedJSONSiblingTypes(t *testing.T) {
	const id = "gc-mixed-metadata"
	durable := &beadslib.Issue{
		ID:        id,
		Title:     "mixed metadata CAS",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.TypeTask,
		Priority:  2,
		Metadata: json.RawMessage(`{
		"lease":"old",
		"bool_sibling":true,
		"number_sibling":42,
		"large_number_sibling":9007199254740993123456789,
		"null_sibling":null,
		"object_sibling":{"nested":"value"},
		"array_sibling":[1,"two",false],
		"string_sibling":"preserved"
	}`),
	}
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return cloneNativeIssueForTest(durable), nil
		},
		updateIssue: func(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
			raw, ok := updates["metadata"].(json.RawMessage)
			if !ok {
				t.Fatalf("metadata update type = %T, want json.RawMessage", updates["metadata"])
			}
			durable.Metadata = append(json.RawMessage(nil), raw...)
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	swapped, err := store.CompareAndSetMetadataKey(id, "lease", "old", "1")
	if err != nil || !swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (true, nil)", swapped, err)
	}
	assertMixedMetadataCASResult(t, durable.Metadata, "9007199254740993123456789")
}

func assertMixedMetadataCASResult(t *testing.T, raw json.RawMessage, wantLargeNumber string) {
	t.Helper()
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode durable metadata: %v", err)
	}
	var want map[string]interface{}
	if err := json.Unmarshal([]byte(`{
		"lease":"1",
		"bool_sibling":true,
		"number_sibling":42,
		"large_number_sibling":9007199254740993123456789,
		"null_sibling":null,
		"object_sibling":{"nested":"value"},
		"array_sibling":[1,"two",false],
		"string_sibling":"preserved"
	}`), &want); err != nil {
		t.Fatalf("decode expected metadata: %v", err)
	}
	var wantLarge interface{}
	if err := json.Unmarshal([]byte(wantLargeNumber), &wantLarge); err != nil {
		t.Fatalf("decode expected large number: %v", err)
	}
	want["large_number_sibling"] = wantLarge
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable metadata = %#v, want %#v; raw=%s", got, want, raw)
	}
	var rawValues map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawValues); err != nil {
		t.Fatalf("decode raw durable metadata: %v", err)
	}
	if value := string(rawValues["large_number_sibling"]); value != wantLargeNumber {
		t.Fatalf("large numeric sibling = %s, want exact %s", value, wantLargeNumber)
	}
}

func TestNativeDoltStoreDeclaresConditionalWriterAndProbesPinnedStorageContract(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	if _, ok := ConditionalWriterFor(store); !ok {
		t.Fatal("NativeDoltStore does not resolve a ConditionalWriter")
	}
	if _, ok := MetadataCASWriterFor(store); !ok {
		t.Fatal("NativeDoltStore does not resolve a MetadataCASWriter")
	}
	if capable, reason := store.probeConditionalWriteCapability(); !capable {
		t.Fatalf("pinned backend capability = false (%s), want true", reason)
	}

	compiledStorage := newNativeDoltStoreForTest(&nativeDoltStorageSpy{})
	if capable, reason := compiledStorage.probeConditionalWriteCapability(); !capable {
		t.Fatalf("compiled Storage capability = false (%s), want true", reason)
	}
}

// TestNativeDoltStoreConditionalWritesResolveForPinnedStorageModes pins the
// mode seam over the compile-time Storage contract. The pinned upstream
// interface requires checked update/close and transactions, so there is no
// runtime "older backend" shape hidden behind the same interface.
func TestNativeDoltStoreConditionalWritesResolveForPinnedStorageModes(t *testing.T) {
	for _, mode := range []gate.Mode{gate.Require, gate.Auto} {
		t.Run(string(mode), func(t *testing.T) {
			store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
			store.stampConditionalWritesMode(mode, false)

			writer, diag, err := ResolveConditionalWriter(store)
			if writer == nil || diag != nil || err != nil {
				t.Fatalf("ResolveConditionalWriter = (%T, %+v, %v), want writer, nil, nil", writer, diag, err)
			}
		})
	}
}

// TestCachingStoreOverNativeDoltStoreForwardsConditionalWrites covers the
// production wrapper shape: the cache must preserve both metadata CAS and the
// guarded whole-row writer advertised by its native backing.
func TestCachingStoreOverNativeDoltStoreForwardsConditionalWrites(t *testing.T) {
	backing := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	cache := NewCachingStore(backing, nil)

	b, err := cache.Create(Bead{Title: "cache-over-native-cas"})
	if err != nil {
		t.Fatal(err)
	}

	writer, ok := MetadataCASWriterFor(cache)
	if !ok {
		t.Fatal("CachingStore over a narrow-CAS backing does not resolve a MetadataCASWriter")
	}
	if swapped, err := writer.CompareAndSetMetadataKey(b.ID, "lease", "", "holder-1"); err != nil || !swapped {
		t.Fatalf("claim through cache: (%v, %v), want (true, nil)", swapped, err)
	}
	// A stale expectation loses cleanly rather than erroring.
	if swapped, err := writer.CompareAndSetMetadataKey(b.ID, "lease", "", "holder-2"); err != nil || swapped {
		t.Fatalf("stale claim through cache: (%v, %v), want (false, nil)", swapped, err)
	}
	// The winner's value is visible through the cache (the CAS evicted, so the
	// next read consults the backing).
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["lease"] != "holder-1" {
		t.Fatalf("lease through cache = %q, want %q", got.Metadata["lease"], "holder-1")
	}

	if capable, reason := cache.probeConditionalWriteCapability(); !capable {
		t.Fatalf("CachingStore reports conditional-write capability = false (%s)", reason)
	}
	conditional, ok := ConditionalWriterFor(cache)
	if !ok {
		t.Fatal("CachingStore over NativeDoltStore does not resolve a ConditionalWriter")
	}
	if err := conditional.UpdateIfMatch(got.ID, got.Revision, UpdateOpts{Labels: []string{"through-cache"}}); err != nil {
		t.Fatalf("related-field UpdateIfMatch through cache: %v", err)
	}
	afterRelated, err := cache.Get(got.ID)
	if err != nil {
		t.Fatalf("Get after related-field UpdateIfMatch: %v", err)
	}
	if !forceCloseContainsString(afterRelated.Labels, "through-cache") || afterRelated.Revision == got.Revision {
		t.Fatalf("related-field update through cache = %+v, want label and fresh revision from %d", afterRelated, got.Revision)
	}
}

// TestNativeDoltStoreMetadataCASIndeterminateCommitIsNotSelfConfirmed models a
// lost commit acknowledgement after the callback's write landed. The durable
// value may match next, but that image does not prove authorship: Gas City must
// return the exported ambiguity sentinel and must not invoke the callback again.
func TestNativeDoltStoreMetadataCASIndeterminateCommitIsNotSelfConfirmed(t *testing.T) {
	storage := &nativeDoltMetadataCASIndeterminateStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{
		Title:    "retry-safe-metadata-cas",
		Metadata: map[string]string{"lease": "unclaimed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	storage.indeterminate = true

	writer, ok := MetadataCASWriterFor(store)
	if !ok {
		t.Fatal("NativeDoltStore does not resolve a MetadataCASWriter")
	}
	swapped, err := writer.CompareAndSetMetadataKey(
		created.ID,
		"lease",
		"unclaimed",
		"holder-1",
	)
	if !errors.Is(err, beadslib.ErrCommitIndeterminate) || swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (false, ErrCommitIndeterminate)", swapped, err)
	}
	if storage.callbackCalls != 1 {
		t.Fatalf("transaction callback calls = %d, want 1", storage.callbackCalls)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value := got.Metadata["lease"]; value != "holder-1" {
		t.Fatalf("durable metadata[%q] = %q, want landed value %q", "lease", value, "holder-1")
	}
}

type nativeDoltMetadataCASIndeterminateStorage struct {
	*nativeDoltMemStorage
	indeterminate bool
	callbackCalls int
}

func (s *nativeDoltMetadataCASIndeterminateStorage) RunInTransaction(
	ctx context.Context,
	_ string,
	fn func(beadslib.Transaction) error,
) error {
	if !s.indeterminate {
		return s.nativeDoltMemStorage.RunInTransaction(ctx, "", fn)
	}
	s.callbackCalls++
	if err := fn(nativeDoltTransactionForTest{storage: s.nativeDoltMemStorage}); err != nil {
		return err
	}
	return fmt.Errorf("commit acknowledgement lost after callback: %w: %w", serializationConflictError(), beadslib.ErrCommitIndeterminate)
}

// TestNativeDoltStoreMetadataCASRollbackRetryRechecksValue models the safe
// retry boundary abc4 exposes: the first callback ran, but a decoded MySQL
// response proves its transaction rolled back. A competitor claims the value
// before the fresh attempt, so this caller must return (false, nil), never the
// first attempt's stale swapped=true result.
func TestNativeDoltStoreMetadataCASRollbackRetryRechecksValue(t *testing.T) {
	storage := &nativeDoltMetadataCASRollbackStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "rollback CAS", Metadata: map[string]string{"lease": "unclaimed"}})
	if err != nil {
		t.Fatal(err)
	}
	storage.targetID = created.ID

	swapped, err := store.CompareAndSetMetadataKey(created.ID, "lease", "unclaimed", "ours")
	if err != nil || swapped {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want (false, nil)", swapped, err)
	}
	if storage.callbackCalls != 2 {
		t.Fatalf("transaction callback calls = %d, want 2", storage.callbackCalls)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["lease"] != "competitor" {
		t.Fatalf("durable lease = %q, want competitor", got.Metadata["lease"])
	}
}

type nativeDoltMetadataCASRollbackStorage struct {
	*nativeDoltMemStorage
	targetID      string
	callbackCalls int
}

func (s *nativeDoltMetadataCASRollbackStorage) RunInTransaction(
	ctx context.Context,
	commitMsg string,
	fn func(beadslib.Transaction) error,
) error {
	s.callbackCalls++
	if s.callbackCalls > 1 {
		return s.nativeDoltMemStorage.RunInTransaction(ctx, commitMsg, fn)
	}

	s.store.mu.Lock()
	seq, beads, deps := s.store.snapshot()
	s.store.mu.Unlock()
	closeSnapshot := s.snapshotCloseProjections()
	if err := fn(nativeDoltTransactionForTest{storage: s.nativeDoltMemStorage}); err != nil {
		return err
	}
	s.store.restoreFrom(seq, beads, deps)
	s.restoreCloseProjections(closeSnapshot)
	if err := s.store.SetMetadata(s.targetID, "lease", "competitor"); err != nil {
		return err
	}
	return serializationConflictError()
}
