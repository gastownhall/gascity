package beads

import (
	"context"
	"encoding/json"
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

type txPreservingBackingStore struct {
	Store
	txCalls     int
	updateCalls int
}

type cacheWriteNotification struct {
	eventType string
	beadID    string
	payload   json.RawMessage
}

func (s *txPreservingBackingStore) Update(id string, opts UpdateOpts) error {
	s.updateCalls++
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	if opts.Title == nil {
		clobbered := ""
		return s.Store.Update(id, UpdateOpts{Title: &clobbered})
	}
	return nil
}

func (s *txPreservingBackingStore) Tx(commitMsg string, fn func(Tx) error) error {
	s.txCalls++
	return s.Store.Tx(commitMsg, fn)
}

func TestCachingStoreTxDelegatesToBackingTxAndRefreshesCache(t *testing.T) {
	t.Parallel()

	backing := &txPreservingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{
		Title:       "preserve title",
		Description: "before",
		Labels:      []string{"keep-label", "drop-label"},
		Metadata:    map[string]string{"existing": "yes"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	description := "after"
	if err := cache.Tx("preserve backing semantics", func(tx Tx) error {
		if err := tx.Update(bead.ID, UpdateOpts{
			Description:  &description,
			Labels:       []string{"new-label"},
			RemoveLabels: []string{"drop-label"},
		}); err != nil {
			return err
		}
		if err := tx.SetMetadataBatch(bead.ID, map[string]string{"tx": "applied"}); err != nil {
			return err
		}
		return tx.Close(bead.ID)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	if backing.txCalls != 1 {
		t.Fatalf("backing.Tx calls = %d, want 1", backing.txCalls)
	}
	if backing.updateCalls != 0 {
		t.Fatalf("backing.Update calls = %d, want 0 direct calls through CachingStore", backing.updateCalls)
	}

	got, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	assertTxPreservedBead(t, got)

	cached, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("cache Get: %v", err)
	}
	assertTxPreservedBead(t, cached)
}

func assertTxPreservedBead(t *testing.T, got Bead) {
	t.Helper()
	if got.Title != "preserve title" {
		t.Fatalf("Title = %q, want preserved title", got.Title)
	}
	if got.Description != "after" {
		t.Fatalf("Description = %q, want after", got.Description)
	}
	if got.Status != "closed" {
		t.Fatalf("Status = %q, want closed", got.Status)
	}
	if got.Metadata["existing"] != "yes" || got.Metadata["tx"] != "applied" {
		t.Fatalf("Metadata = %#v, want existing=yes and tx=applied", got.Metadata)
	}
	if !stringSliceContains(got.Labels, "keep-label") || !stringSliceContains(got.Labels, "new-label") || stringSliceContains(got.Labels, "drop-label") {
		t.Fatalf("Labels = %#v, want keep-label and new-label without drop-label", got.Labels)
	}
}

func TestCachingStoreSetMetadataBatchNotifiesBeadUpdated(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "metadata"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var notifications []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notifications = append(notifications, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.SetMetadataBatch(bead.ID, map[string]string{"review": "fixed"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want 1: %#v", len(notifications), notifications)
	}
	if notifications[0].eventType != "bead.updated" || notifications[0].beadID != bead.ID {
		t.Fatalf("notification = %#v, want bead.updated for %s", notifications[0], bead.ID)
	}
	updated, _, err := decodeCacheEvent(notifications[0].payload)
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if updated.Metadata["review"] != "fixed" {
		t.Fatalf("notification metadata = %#v, want review=fixed", updated.Metadata)
	}
}

func TestCachingStoreDependencyWritesNotifyBeadUpdatedWithDeps(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	target, err := backing.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	blocker, err := backing.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	var notifications []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notifications = append(notifications, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if err := cache.DepRemove(target.ID, blocker.ID); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}

	if len(notifications) != 2 {
		t.Fatalf("notifications = %d, want 2: %#v", len(notifications), notifications)
	}
	added, _, err := decodeCacheEvent(notifications[0].payload)
	if err != nil {
		t.Fatalf("decode add notification: %v", err)
	}
	if notifications[0].eventType != "bead.updated" || len(added.Dependencies) != 1 || added.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("add notification = %#v bead=%+v, want dependency snapshot", notifications[0], added)
	}
	removed, _, err := decodeCacheEvent(notifications[1].payload)
	if err != nil {
		t.Fatalf("decode remove notification: %v", err)
	}
	if notifications[1].eventType != "bead.updated" || len(removed.Dependencies) != 0 {
		t.Fatalf("remove notification = %#v bead=%+v, want empty dependency snapshot", notifications[1], removed)
	}
}

func TestCachingStoreDeleteNotifiesBeadDeleted(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "delete"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var notifications []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notifications = append(notifications, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.Delete(bead.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want 1: %#v", len(notifications), notifications)
	}
	if notifications[0].eventType != "bead.deleted" || notifications[0].beadID != bead.ID {
		t.Fatalf("notification = %#v, want bead.deleted for %s", notifications[0], bead.ID)
	}
	deleted, _, err := decodeCacheEvent(notifications[0].payload)
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if deleted.ID != bead.ID || deleted.Title != "delete" {
		t.Fatalf("deleted payload = %+v, want deleted bead snapshot", deleted)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

func TestCachingStoreSetMetadataFallsThroughWhenCacheStateCannotProveNoop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state cacheState
	}{
		{name: "uninitialized", state: cacheUninitialized},
		{name: "degraded", state: cacheDegraded},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/single", func(t *testing.T) {
			t.Parallel()

			backing := &countingBackingStore{Store: NewMemStore()}
			bead := createBeadWithMetadata(t, backing, map[string]string{"foo": "bar"})
			cache := staleMatchingMetadataCache(backing, bead, tt.state)
			backing.setMetadataCalls = 0

			if err := cache.SetMetadata(bead.ID, "foo", "bar"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			if backing.setMetadataCalls != 1 {
				t.Fatalf("backing.SetMetadata called %d times; want 1", backing.setMetadataCalls)
			}
		})

		t.Run(tt.name+"/batch", func(t *testing.T) {
			t.Parallel()

			backing := &countingBackingStore{Store: NewMemStore()}
			bead := createBeadWithMetadata(t, backing, map[string]string{"foo": "bar", "baz": "qux"})
			cache := staleMatchingMetadataCache(backing, bead, tt.state)
			backing.setMetadataBatchCalls = 0

			if err := cache.SetMetadataBatch(bead.ID, map[string]string{"foo": "bar", "baz": "qux"}); err != nil {
				t.Fatalf("SetMetadataBatch: %v", err)
			}
			if backing.setMetadataBatchCalls != 1 {
				t.Fatalf("backing.SetMetadataBatch called %d times; want 1", backing.setMetadataBatchCalls)
			}
		})
	}
}

func createBeadWithMetadata(t *testing.T, backing Store, metadata map[string]string) Bead {
	t.Helper()

	bead, err := backing.Create(Bead{Title: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.SetMetadataBatch(bead.ID, metadata); err != nil {
		t.Fatalf("seed SetMetadataBatch: %v", err)
	}
	bead, err = backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return bead
}

func staleMatchingMetadataCache(backing Store, bead Bead, state cacheState) *CachingStore {
	cache := NewCachingStoreForTest(backing, nil)
	cache.mu.Lock()
	cache.beads[bead.ID] = cloneBead(bead)
	cache.state = state
	cache.mu.Unlock()
	return cache
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

// TestCachingStoreUpdateSkipsBackingPerFieldMatch is the per-field
// short-circuit coverage requested in gastownhall/gascity#2199. The original
// PR #2159 exercised Assignee + Labels-mismatch + cache-miss only; the
// remaining 6 field branches in updateMatchesCached were asserted by
// inspection. This table-driven test pins the short-circuit behavior for
// each field independently so a future refactor of any single check
// surfaces in CI.
func TestCachingStoreUpdateSkipsBackingPerFieldMatch(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name string
		seed Bead
		opts UpdateOpts
	}
	strPtr := func(s string) *string { return &s }
	intPtr := func(i int) *int { return &i }

	cases := []fieldCase{
		{
			name: "Title",
			seed: Bead{Title: "pinned"},
			opts: UpdateOpts{Title: strPtr("pinned")},
		},
		{
			name: "Status",
			seed: Bead{Title: "x", Status: "open"},
			opts: UpdateOpts{Status: strPtr("open")},
		},
		{
			name: "Type",
			seed: Bead{Title: "x", Type: "task"},
			opts: UpdateOpts{Type: strPtr("task")},
		},
		{
			name: "Priority",
			seed: Bead{Title: "x", Priority: intPtr(2)},
			opts: UpdateOpts{Priority: intPtr(2)},
		},
		{
			name: "Description",
			seed: Bead{Title: "x", Description: "body"},
			opts: UpdateOpts{Description: strPtr("body")},
		},
		{
			name: "ParentID",
			seed: Bead{Title: "x", ParentID: "gc-parent"},
			opts: UpdateOpts{ParentID: strPtr("gc-parent")},
		},
		{
			name: "Metadata",
			seed: Bead{Title: "x", Metadata: map[string]string{"k": "v"}},
			opts: UpdateOpts{Metadata: map[string]string{"k": "v"}},
		},
		{
			name: "Labels-present",
			seed: Bead{Title: "x", Labels: []string{"a", "b"}},
			opts: UpdateOpts{Labels: []string{"a"}},
		},
		{
			name: "RemoveLabels-absent",
			seed: Bead{Title: "x", Labels: []string{"a"}},
			opts: UpdateOpts{RemoveLabels: []string{"z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backing := &countingBackingStore{Store: NewMemStore()}
			bead, err := backing.Create(tc.seed)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			cache := NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			backing.updateCalls = 0

			if err := cache.Update(bead.ID, tc.opts); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if backing.updateCalls != 0 {
				t.Errorf("backing.Update called %d times; want 0 (%s value-match must short-circuit)",
					backing.updateCalls, tc.name)
			}
		})
	}
}

// TestCachingStoreUpdateFallsThroughPerFieldMismatch is the mismatch-side
// companion to TestCachingStoreUpdateSkipsBackingPerFieldMatch. Each
// subtest asserts that a real change in the named field forces the
// backing call — guarding the matcher against accidentally returning true
// when a single field actually differs.
func TestCachingStoreUpdateFallsThroughPerFieldMismatch(t *testing.T) {
	t.Parallel()

	type fieldCase struct {
		name string
		seed Bead
		opts UpdateOpts
	}
	strPtr := func(s string) *string { return &s }
	intPtr := func(i int) *int { return &i }

	cases := []fieldCase{
		{
			name: "Title",
			seed: Bead{Title: "before"},
			opts: UpdateOpts{Title: strPtr("after")},
		},
		{
			name: "Status",
			seed: Bead{Title: "x", Status: "open"},
			opts: UpdateOpts{Status: strPtr("closed")},
		},
		{
			name: "Type",
			seed: Bead{Title: "x", Type: "task"},
			opts: UpdateOpts{Type: strPtr("epic")},
		},
		{
			name: "Priority",
			seed: Bead{Title: "x", Priority: intPtr(2)},
			opts: UpdateOpts{Priority: intPtr(3)},
		},
		{
			name: "Priority-nil-cached",
			seed: Bead{Title: "x"},
			opts: UpdateOpts{Priority: intPtr(2)},
		},
		{
			name: "Description",
			seed: Bead{Title: "x", Description: "before"},
			opts: UpdateOpts{Description: strPtr("after")},
		},
		{
			name: "ParentID",
			seed: Bead{Title: "x", ParentID: "gc-a"},
			opts: UpdateOpts{ParentID: strPtr("gc-b")},
		},
		{
			name: "Metadata-value",
			seed: Bead{Title: "x", Metadata: map[string]string{"k": "old"}},
			opts: UpdateOpts{Metadata: map[string]string{"k": "new"}},
		},
		{
			name: "Metadata-missing-key",
			seed: Bead{Title: "x"},
			opts: UpdateOpts{Metadata: map[string]string{"k": "v"}},
		},
		{
			name: "RemoveLabels-present",
			seed: Bead{Title: "x", Labels: []string{"a", "b"}},
			opts: UpdateOpts{RemoveLabels: []string{"a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backing := &countingBackingStore{Store: NewMemStore()}
			bead, err := backing.Create(tc.seed)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			cache := NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			backing.updateCalls = 0

			if err := cache.Update(bead.ID, tc.opts); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if backing.updateCalls != 1 {
				t.Errorf("backing.Update called %d times; want 1 (%s real change must propagate)",
					backing.updateCalls, tc.name)
			}
		})
	}
}
