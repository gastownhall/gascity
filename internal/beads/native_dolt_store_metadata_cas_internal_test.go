package beads

import (
	"context"
	"encoding/json"
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
	if _, ok := ConditionalWriterFor(cache); !ok {
		t.Fatal("CachingStore over NativeDoltStore does not resolve a ConditionalWriter")
	}
}

// TestNativeDoltStoreMetadataCASRetryDoesNotLeakPriorAttemptResult models the
// whole-callback retry performed by beads/Dolt RunInTransaction. A first
// callback reaches UpdateIssue, then the retry observes a competitor's value.
// The durable result is a lost race, regardless of what the abandoned callback
// wrote before the retry.
func TestNativeDoltStoreMetadataCASRetryDoesNotLeakPriorAttemptResult(t *testing.T) {
	storage := &nativeDoltMetadataCASRetryStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		key:                  "lease",
		competitor:           "holder-2",
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{
		Title:    "retry-safe-metadata-cas",
		Metadata: map[string]string{"lease": "unclaimed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	storage.id = created.ID
	storage.retryCAS = true

	writer, ok := MetadataCASWriterFor(store)
	if !ok {
		t.Fatal("NativeDoltStore does not resolve a MetadataCASWriter")
	}
	swapped, err := writer.CompareAndSetMetadataKey(
		created.ID,
		storage.key,
		"unclaimed",
		"holder-1",
	)
	if err != nil {
		t.Fatalf("CompareAndSetMetadataKey: %v", err)
	}
	if swapped {
		t.Fatal("CompareAndSetMetadataKey = true after retry lost to competitor")
	}
	if storage.callbackCalls != 2 {
		t.Fatalf("transaction callback calls = %d, want 2", storage.callbackCalls)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if value := got.Metadata[storage.key]; value != storage.competitor {
		t.Fatalf("durable metadata[%q] = %q, want competitor %q", storage.key, value, storage.competitor)
	}
}

type nativeDoltMetadataCASRetryStorage struct {
	*nativeDoltMemStorage
	id            string
	key           string
	competitor    string
	retryCAS      bool
	callbackCalls int
}

func (s *nativeDoltMetadataCASRetryStorage) RunInTransaction(
	ctx context.Context,
	_ string,
	fn func(beadslib.Transaction) error,
) error {
	if !s.retryCAS {
		return s.nativeDoltMemStorage.RunInTransaction(ctx, "", fn)
	}

	tx := nativeDoltTransactionForTest{storage: s.nativeDoltMemStorage}
	// Snapshot the durable state before the first callback. The callback's
	// UpdateIssue is deliberately rolled back below to model a commit-phase
	// failure: it may have set the caller's local result flag, but it never
	// linearized in the store.
	s.store.mu.Lock()
	seq, beads, deps := s.store.snapshot()
	s.store.mu.Unlock()
	s.callbackCalls++
	if err := fn(tx); err != nil {
		return err
	}
	s.store.restoreFrom(seq, beads, deps)

	raw, err := metadataRawFromMap(map[string]string{s.key: s.competitor})
	if err != nil {
		return err
	}
	if err := s.UpdateIssue(
		ctx,
		s.id,
		map[string]interface{}{"metadata": raw},
		"competitor",
	); err != nil {
		return err
	}

	s.callbackCalls++
	return fn(tx)
}
