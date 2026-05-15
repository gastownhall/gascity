package beads

import (
	"context"
	"testing"
)

// countingBackingStore wraps a Store and counts SetMetadata /
// SetMetadataBatch / Update / Close invocations so tests can assert when
// CachingStore short-circuits a no-op write before the backing call.
type countingBackingStore struct {
	Store
	setMetadataCalls      int
	setMetadataBatchCalls int
	updateCalls           int
	closeCalls            int
}

func (c *countingBackingStore) SetMetadata(id, key, value string) error {
	c.setMetadataCalls++
	return c.Store.SetMetadata(id, key, value)
}

func (c *countingBackingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	c.setMetadataBatchCalls++
	return c.Store.SetMetadataBatch(id, kvs)
}

func (c *countingBackingStore) Update(id string, opts UpdateOpts) error {
	c.updateCalls++
	return c.Store.Update(id, opts)
}

func (c *countingBackingStore) Close(id string) error {
	c.closeCalls++
	return c.Store.Close(id)
}

// TestCachingStoreSetMetadataSkipsBackingWhenCachedValueMatches verifies that
// SetMetadata short-circuits before the backing call when the cached bead
// already has metadata[key]==value. Without this guard, no-op writes still
// fire bd's on_update hook and emit a bead.updated event.
func TestCachingStoreSetMetadataSkipsBackingWhenCachedValueMatches(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.SetMetadata(bead.ID, "foo", "bar"); err != nil {
		t.Fatalf("seed SetMetadata: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.setMetadataCalls = 0

	if err := cache.SetMetadata(bead.ID, "foo", "bar"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if backing.setMetadataCalls != 0 {
		t.Errorf("backing.SetMetadata called %d times; want 0 (no-op write must short-circuit)",
			backing.setMetadataCalls)
	}
}

// TestCachingStoreSetMetadataFallsThroughOnValueMismatch verifies that a
// real value change still propagates to the backing store.
func TestCachingStoreSetMetadataFallsThroughOnValueMismatch(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.SetMetadata(bead.ID, "foo", "old"); err != nil {
		t.Fatalf("seed SetMetadata: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.setMetadataCalls = 0

	if err := cache.SetMetadata(bead.ID, "foo", "new"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if backing.setMetadataCalls != 1 {
		t.Errorf("backing.SetMetadata called %d times; want 1 (real change must propagate)",
			backing.setMetadataCalls)
	}
}

// TestCachingStoreSetMetadataFallsThroughOnCacheMiss verifies that
// SetMetadata calls the backing store when the cache has no entry for the
// bead — without a primed copy we cannot prove the write is a no-op.
func TestCachingStoreSetMetadataFallsThroughOnCacheMiss(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	bead, err := backing.Create(Bead{Title: "post-prime"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.setMetadataCalls = 0

	if err := cache.SetMetadata(bead.ID, "foo", "bar"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if backing.setMetadataCalls != 1 {
		t.Errorf("backing.SetMetadata called %d times; want 1 (cache miss must fall through)",
			backing.setMetadataCalls)
	}
}

// TestCachingStoreSetMetadataBatchSkipsBackingWhenAllCachedValuesMatch
// verifies that SetMetadataBatch short-circuits when every kv pair already
// matches the cached metadata.
func TestCachingStoreSetMetadataBatchSkipsBackingWhenAllCachedValuesMatch(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for k, v := range map[string]string{"foo": "1", "bar": "2", "baz": "3"} {
		if err := backing.SetMetadata(bead.ID, k, v); err != nil {
			t.Fatalf("seed SetMetadata(%s): %v", k, err)
		}
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.setMetadataBatchCalls = 0

	if err := cache.SetMetadataBatch(bead.ID, map[string]string{"foo": "1", "bar": "2"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if backing.setMetadataBatchCalls != 0 {
		t.Errorf("backing.SetMetadataBatch called %d times; want 0 (all-match must short-circuit)",
			backing.setMetadataBatchCalls)
	}
}

// TestCachingStoreSetMetadataBatchFallsThroughOnAnyMismatch verifies that
// even one mismatching kv forces the backing call — partial matches do not
// suffice to skip the write.
func TestCachingStoreSetMetadataBatchFallsThroughOnAnyMismatch(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for k, v := range map[string]string{"foo": "1", "bar": "2"} {
		if err := backing.SetMetadata(bead.ID, k, v); err != nil {
			t.Fatalf("seed SetMetadata(%s): %v", k, err)
		}
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.setMetadataBatchCalls = 0

	// foo matches the cached value, bar does not. The mismatch must force
	// the full batch to the backing store.
	if err := cache.SetMetadataBatch(bead.ID, map[string]string{"foo": "1", "bar": "DIFFERENT"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if backing.setMetadataBatchCalls != 1 {
		t.Errorf("backing.SetMetadataBatch called %d times; want 1 (mismatch must propagate)",
			backing.setMetadataBatchCalls)
	}
}

// TestCachingStoreSetMetadataBatchEmptyKVsIsNoop verifies that an empty kvs
// map returns nil immediately without calling the backing store. This is
// the early-return branch before metadataAlreadyMatchesCached.
func TestCachingStoreSetMetadataBatchEmptyKVsIsNoop(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.setMetadataBatchCalls = 0

	if err := cache.SetMetadataBatch(bead.ID, map[string]string{}); err != nil {
		t.Fatalf("SetMetadataBatch(empty): %v", err)
	}
	if backing.setMetadataBatchCalls != 0 {
		t.Errorf("backing.SetMetadataBatch called %d times; want 0 (empty kvs must short-circuit)",
			backing.setMetadataBatchCalls)
	}
}

// TestCachingStoreUpdateSkipsBackingWhenAllFieldsMatch verifies that Update
// short-circuits before the backing call when every non-nil opts field
// already matches the cached bead. Without this guard the reconciler's
// per-tick Update calls fire bd subprocesses + post-Get refreshes even when
// the payload is identical. See gastownhall/gascity#1978 Phase 1.
func TestCachingStoreUpdateSkipsBackingWhenAllFieldsMatch(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test", Assignee: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.updateCalls = 0

	assignee := "alice"
	if err := cache.Update(bead.ID, UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if backing.updateCalls != 0 {
		t.Errorf("backing.Update called %d times; want 0 (no-op update must short-circuit)",
			backing.updateCalls)
	}
}

// TestCachingStoreUpdateFallsThroughOnValueMismatch verifies that a real
// field change still propagates to the backing store.
func TestCachingStoreUpdateFallsThroughOnValueMismatch(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test", Assignee: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.updateCalls = 0

	assignee := "bob"
	if err := cache.Update(bead.ID, UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if backing.updateCalls != 1 {
		t.Errorf("backing.Update called %d times; want 1 (real change must propagate)",
			backing.updateCalls)
	}
}

// TestCachingStoreUpdateFallsThroughOnCacheMiss verifies that Update calls
// the backing store when the cache has no entry for the bead — without a
// primed copy we cannot prove the write is a no-op.
func TestCachingStoreUpdateFallsThroughOnCacheMiss(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	bead, err := backing.Create(Bead{Title: "post-prime", Assignee: "alice"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.updateCalls = 0

	assignee := "alice"
	if err := cache.Update(bead.ID, UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if backing.updateCalls != 1 {
		t.Errorf("backing.Update called %d times; want 1 (cache miss must fall through)",
			backing.updateCalls)
	}
}

// TestCachingStoreUpdateFallsThroughOnLabelMismatch verifies that a Labels
// opt requesting a label not yet on the bead still propagates to the backing
// store.
func TestCachingStoreUpdateFallsThroughOnLabelMismatch(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test", Labels: []string{"existing"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.updateCalls = 0

	if err := cache.Update(bead.ID, UpdateOpts{Labels: []string{"new-label"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if backing.updateCalls != 1 {
		t.Errorf("backing.Update called %d times; want 1 (new label must propagate)",
			backing.updateCalls)
	}
}

// TestCachingStoreCloseSkipsBackingWhenAlreadyClosed verifies that Close
// short-circuits before the backing call when the cached bead is already
// closed. The cache only holds active beads after Prime, so the close has
// to happen through CachingStore first to seed the closed status into the
// cache. See gastownhall/gascity#1978 Phase 1.
func TestCachingStoreCloseSkipsBackingWhenAlreadyClosed(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// First close: open → closed, must propagate.
	if err := cache.Close(bead.ID); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if backing.closeCalls != 1 {
		t.Fatalf("backing.Close after first close = %d, want 1", backing.closeCalls)
	}
	backing.closeCalls = 0

	// Second close on the already-closed bead must short-circuit. The
	// reconciler / cleanup paths sometimes re-close the same bead on
	// retry; that should not generate fresh bd subprocess traffic.
	if err := cache.Close(bead.ID); err != nil {
		t.Fatalf("repeat Close: %v", err)
	}
	if backing.closeCalls != 0 {
		t.Errorf("backing.Close called %d times on repeat close; want 0 (already-closed must short-circuit)",
			backing.closeCalls)
	}
}

// TestCachingStoreCloseFallsThroughWhenOpen verifies that a real close still
// propagates to the backing store.
func TestCachingStoreCloseFallsThroughWhenOpen(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.closeCalls = 0

	if err := cache.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if backing.closeCalls != 1 {
		t.Errorf("backing.Close called %d times; want 1 (open->closed must propagate)",
			backing.closeCalls)
	}
}

// TestCachingStoreCloseFallsThroughOnCacheMiss verifies that Close calls the
// backing store when the cache has no entry for the bead.
func TestCachingStoreCloseFallsThroughOnCacheMiss(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	bead, err := backing.Create(Bead{Title: "post-prime"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing.closeCalls = 0

	if err := cache.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if backing.closeCalls != 1 {
		t.Errorf("backing.Close called %d times; want 1 (cache miss must fall through)",
			backing.closeCalls)
	}
}
