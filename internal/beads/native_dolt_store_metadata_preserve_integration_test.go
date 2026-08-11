//go:build integration

package beads

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// openRealNativeDoltStoreForMetadata opens a NativeDoltStore over REAL
// upstream native storage and returns the storage handle alongside it, so a
// test can seed and inspect the raw metadata document that the store actually
// persisted. The store's own API is map[string]string end to end, so it cannot
// express — or observe — a non-string metadata value on its own.
func openRealNativeDoltStoreForMetadata(t *testing.T, actor string) (*NativeDoltStore, beadslib.Storage) {
	t.Helper()
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, actor, "gc"), storage
}

// seedRawMetadata writes a metadata document verbatim, bypassing the store's
// map[string]string API so the bead ends up holding genuine JSON types — the
// shape bd itself produces from `--set-metadata flag=true`.
func seedRawMetadata(t *testing.T, storage beadslib.Storage, id, document, actor string) {
	t.Helper()
	err := storage.UpdateIssue(context.Background(), id, map[string]interface{}{
		"metadata": json.RawMessage(document),
	}, actor)
	if err != nil {
		t.Fatalf("seeding raw metadata: %v", err)
	}
}

// readRawMetadata returns the metadata document as actually persisted.
func readRawMetadata(t *testing.T, storage beadslib.Storage, id string) map[string]any {
	t.Helper()
	issue, err := storage.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue == nil {
		t.Fatalf("bead %q not found", id)
	}
	var decoded map[string]any
	if err := json.Unmarshal(issue.Metadata, &decoded); err != nil {
		t.Fatalf("unmarshaling persisted metadata %s: %v", issue.Metadata, err)
	}
	return decoded
}

// assertTypesPreserved checks the seeded values survived a write round-trip
// against real storage with their JSON types intact.
func assertTypesPreserved(t *testing.T, got map[string]any, wroteKey string) {
	t.Helper()
	if flag, ok := got["flag"].(bool); !ok || !flag {
		t.Errorf("flag = %#v (%T), want bool true — an untouched key was retyped", got["flag"], got["flag"])
	}
	if num, ok := got["num"].(float64); !ok || num != 42 {
		t.Errorf("num = %#v (%T), want JSON number 42 — an untouched key was retyped", got["num"], got["num"])
	}
	if nested, ok := got["nested"].(map[string]any); !ok {
		t.Errorf("nested = %#v (%T), want JSON object — an untouched key was retyped", got["nested"], got["nested"])
	} else if a, ok := nested["a"].(float64); !ok || a != 1 {
		t.Errorf("nested.a = %#v, want JSON number 1", nested["a"])
	}
	if v, ok := got[wroteKey].(string); !ok || v != "x" {
		t.Errorf("%s = %#v, want the named key written as %q", wroteKey, got[wroteKey], "x")
	}
}

const seededMetadataDocument = `{"flag": true, "num": 42, "nested": {"a": 1}}`

// TestNativeDoltStoreUpdatePreservesMetadataTypesAgainstRealDolt is the
// end-to-end proof: a bead holding genuine JSON types keeps them after an
// Update names an unrelated key, verified by reading back the document real
// storage persisted rather than the map the store handed to the driver.
func TestNativeDoltStoreUpdatePreservesMetadataTypesAgainstRealDolt(t *testing.T) {
	store, storage := openRealNativeDoltStoreForMetadata(t, "metadata-preserve")

	b, err := store.Create(Bead{Title: "real-dolt-metadata-preserve"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedRawMetadata(t, storage, b.ID, seededMetadataDocument, "metadata-preserve")

	if err := store.Update(b.ID, UpdateOpts{Metadata: map[string]string{"other": "x"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	assertTypesPreserved(t, readRawMetadata(t, storage, b.ID), "other")
}

// TestNativeDoltStoreSetMetadataBatchPreservesTypesAgainstRealDolt covers the
// SetMetadataBatch write path against real storage.
func TestNativeDoltStoreSetMetadataBatchPreservesTypesAgainstRealDolt(t *testing.T) {
	store, storage := openRealNativeDoltStoreForMetadata(t, "metadata-preserve-batch")

	b, err := store.Create(Bead{Title: "real-dolt-metadata-preserve-batch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedRawMetadata(t, storage, b.ID, seededMetadataDocument, "metadata-preserve-batch")

	if err := store.SetMetadataBatch(b.ID, map[string]string{"other": "x"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	assertTypesPreserved(t, readRawMetadata(t, storage, b.ID), "other")
}

// TestNativeDoltStoreCASPreservesOtherMetadataTypesAgainstRealDolt covers the
// compare-and-set write path: swapping one key must not retype its neighbours,
// and the compare itself must still match a stored non-string value by its
// JSON text.
func TestNativeDoltStoreCASPreservesOtherMetadataTypesAgainstRealDolt(t *testing.T) {
	store, storage := openRealNativeDoltStoreForMetadata(t, "metadata-preserve-cas")

	b, err := store.Create(Bead{Title: "real-dolt-metadata-preserve-cas"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedRawMetadata(t, storage, b.ID, `{"flag": true, "num": 42, "nested": {"a": 1}, "lease": "held"}`, "metadata-preserve-cas")

	ok, err := store.CompareAndSetMetadataKey(b.ID, "lease", "held", "x")
	if err != nil || !ok {
		t.Fatalf("CompareAndSetMetadataKey: (%v, %v), want (true, nil)", ok, err)
	}

	assertTypesPreserved(t, readRawMetadata(t, storage, b.ID), "lease")
}

// TestNativeDoltStoreCASComparesAgainstStoredNonStringValue pins that the CAS
// compare still resolves a stored JSON number against its text form, which is
// how the pre-existing behavior read it. Only the write path changed.
func TestNativeDoltStoreCASComparesAgainstStoredNonStringValue(t *testing.T) {
	store, storage := openRealNativeDoltStoreForMetadata(t, "metadata-cas-nonstring")

	b, err := store.Create(Bead{Title: "real-dolt-metadata-cas-nonstring"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seedRawMetadata(t, storage, b.ID, `{"attempt": 3}`, "metadata-cas-nonstring")

	ok, err := store.CompareAndSetMetadataKey(b.ID, "attempt", "3", "4")
	if err != nil || !ok {
		t.Fatalf("CAS against stored JSON number: (%v, %v), want (true, nil) — compare semantics changed", ok, err)
	}
	got := readRawMetadata(t, storage, b.ID)
	if attempt, ok := got["attempt"].(string); !ok || attempt != "4" {
		t.Errorf("attempt = %#v, want the swapped-in string %q", got["attempt"], "4")
	}
}
