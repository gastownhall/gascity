package beads

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

// countingBackingStore wraps a Store and counts SetMetadata /
// SetMetadataBatch / Update / Close invocations so tests can assert that a
// CachingStore write reaches the backing.
type countingBackingStore struct {
	Store
	setMetadataCalls      int
	setMetadataBatchCalls int
	updateCalls           int
	closeCalls            int
	releaseIfCurrentCalls int
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

func (c *countingBackingStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	c.releaseIfCurrentCalls++
	releaser, ok := c.Store.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
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

type releaseRefreshFailOnceStore struct {
	Store
	failNextGet bool
}

type ambiguousMetadataBatchStore struct {
	Store
	err        error
	commitKeys []string
}

func (s *ambiguousMetadataBatchStore) SetMetadataBatch(id string, kvs map[string]string) error {
	committed := make(map[string]string, len(s.commitKeys))
	for _, key := range s.commitKeys {
		committed[key] = kvs[key]
	}
	if len(committed) > 0 {
		if err := s.Store.SetMetadataBatch(id, committed); err != nil {
			return err
		}
	}
	return s.err
}

func (s *releaseRefreshFailOnceStore) Get(id string) (Bead, error) {
	if s.failNextGet {
		s.failNextGet = false
		return Bead{}, errors.New("injected refresh failure")
	}
	return s.Store.Get(id)
}

func (s *releaseRefreshFailOnceStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if released && err == nil {
		s.failNextGet = true
	}
	return released, err
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

func TestCachingStoreTxCloseClearsDependentProjectedIsBlocked(t *testing.T) {
	t.Parallel()

	blockedProjection := true
	backing := NewMemStore()
	blocker, err := backing.Create(Bead{
		Title:  "blocker",
		Status: "open",
		Type:   "task",
	})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	blocked, err := backing.Create(Bead{
		Title:     "blocked",
		Status:    "open",
		Type:      "task",
		Needs:     []string{blocker.ID},
		IsBlocked: &blockedProjection,
	})
	if err != nil {
		t.Fatalf("Create blocked: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.Tx("close blocker", func(tx Tx) error {
		return tx.Close(blocker.ID)
	}); err != nil {
		t.Fatalf("Tx close blocker: %v", err)
	}

	ready, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable after tx close")
	}
	readyByID := make(map[string]bool, len(ready))
	for _, bead := range ready {
		readyByID[bead.ID] = true
	}
	if !readyByID[blocked.ID] {
		t.Fatalf("CachedReady after tx close ids = %v, want dependent unblocked by closed blocker", readyByID)
	}

	got, err := cache.Get(blocked.ID)
	if err != nil {
		t.Fatalf("Get blocked after tx close: %v", err)
	}
	if got.IsBlocked != nil {
		t.Fatalf("dependent IsBlocked after tx close = %v, want nil fallback to cached deps", got.IsBlocked)
	}
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

func TestCachingStoreSetMetadataBatchErrorFencesStaleRow(t *testing.T) {
	patch := map[string]string{"state": "asleep", "reason": "healed"}
	for _, tc := range []struct {
		name       string
		commitKeys []string
		wantState  string
		wantReason string
	}{
		{name: "rejected", wantState: "active", wantReason: "old"},
		{name: "partially committed", commitKeys: []string{"state"}, wantState: "asleep", wantReason: "old"},
		{name: "fully committed", commitKeys: []string{"state", "reason"}, wantState: "asleep", wantReason: "healed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemStore()
			bead, err := mem.Create(Bead{
				Title:    "worker",
				Type:     "session",
				Metadata: map[string]string{"state": "active", "reason": "old"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			writeErr := errors.New("ambiguous metadata batch")
			backing := &ambiguousMetadataBatchStore{
				Store:      mem,
				err:        writeErr,
				commitKeys: tc.commitKeys,
			}
			notifications := 0
			cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) {
				notifications++
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			cache.mu.RLock()
			startSeq := cache.mutationSeq
			cache.mu.RUnlock()

			if err := cache.SetMetadataBatch(bead.ID, patch); !errors.Is(err, writeErr) {
				t.Fatalf("SetMetadataBatch error = %v, want %v", err, writeErr)
			}

			cache.mu.RLock()
			fence := cache.beadSeq[bead.ID]
			_, dirty := cache.dirty[bead.ID]
			_, local := cache.localBeadAt[bead.ID]
			cache.mu.RUnlock()
			if fence <= startSeq || !dirty || local {
				t.Fatalf("ambiguity fence = seq:%d start:%d dirty:%v local:%v", fence, startSeq, dirty, local)
			}
			if notifications != 0 {
				t.Fatalf("notifications = %d, want 0 for an unconfirmed write", notifications)
			}
			query := ListQuery{Type: "session"}
			if _, ok := cache.CachedList(query); ok {
				t.Fatal("CachedList served a row whose backing write outcome is unknown")
			}

			rows, err := cache.List(query)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(rows) != 1 || rows[0].Metadata["state"] != tc.wantState || rows[0].Metadata["reason"] != tc.wantReason {
				t.Fatalf("List metadata = %#v, want state=%q reason=%q", rows, tc.wantState, tc.wantReason)
			}
			if rows, ok := cache.CachedList(query); !ok || len(rows) != 1 ||
				rows[0].Metadata["state"] != tc.wantState || rows[0].Metadata["reason"] != tc.wantReason {
				t.Fatalf("CachedList after reread = %#v, ok=%v", rows, ok)
			}
		})
	}
}

func TestCachingStoreReleaseIfCurrentDelegatesAndRefreshesCache(t *testing.T) {
	t.Parallel()

	status := "in_progress"
	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "task", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	events = nil

	released, err := cache.ReleaseIfCurrent(bead.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	if backing.releaseIfCurrentCalls != 1 {
		t.Fatalf("backing ReleaseIfCurrent calls = %d, want 1", backing.releaseIfCurrentCalls)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("cache Get: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("cached bead = %+v, want open and unassigned", got)
	}
	if !stringSliceContains(events, "bead.updated:"+bead.ID) {
		t.Fatalf("events = %v, want bead.updated for released bead", events)
	}
}

func TestCachingStoreReleaseIfCurrentKeepsDirtyWhenRefreshFails(t *testing.T) {
	t.Parallel()

	status := "in_progress"
	backing := &releaseRefreshFailOnceStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "task", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	released, err := cache.ReleaseIfCurrent(bead.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent released = false, want true")
	}
	cache.mu.Lock()
	_, dirty := cache.dirty[bead.ID]
	cache.mu.Unlock()
	if !dirty {
		t.Fatal("released bead was not kept dirty after refresh failure")
	}

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("cache Get after dirty refresh: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("cached bead after dirty refresh = %+v, want open and unassigned", got)
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

// TestCachingStoreSetMetadataBatchEmptyKVsIsNoop verifies that an empty kvs
// map returns nil immediately without calling the backing store.
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

func TestCachingStoreCloseAdoptsFreshBackingRead(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "close-adopt"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := cache.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fresh, err := backing.Get(b.ID)
	if err != nil {
		t.Fatalf("backing Get after close: %v", err)
	}
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("cache Get after close: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("cached status after Close = %q, want %q", got.Status, "closed")
	}
	if got.Revision != fresh.Revision {
		t.Fatalf("cached revision after Close = %d, backing = %d; the successful refresh read must be adopted, "+
			"or a Get→conditional-write consumer fences against a revision that no longer exists",
			got.Revision, fresh.Revision)
	}
}

func TestCachingStoreReopenAdoptsFreshBackingRead(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "reopen-adopt"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := cache.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := cache.Reopen(b.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	fresh, err := backing.Get(b.ID)
	if err != nil {
		t.Fatalf("backing Get after reopen: %v", err)
	}
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("cache Get after reopen: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("cached status after Reopen = %q, want %q", got.Status, "open")
	}
	if got.Revision != fresh.Revision {
		t.Fatalf("cached revision after Reopen = %d, backing = %d; the successful refresh read must be adopted",
			got.Revision, fresh.Revision)
	}
}

func TestCachingStoreReopenClearsStatusBasedDeferralWhenRefreshFails(t *testing.T) {
	t.Parallel()

	backing := &releaseRefreshFailOnceStore{Store: NewMemStore()}
	created, err := backing.Create(Bead{
		Title:                "deferred",
		Status:               "open",
		IndefinitelyDeferred: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	// The fixture reaches the cache deferred only because MemStore.Create
	// seeds Status directly instead of taking an explicit transition, which
	// clears the marker. Pin that here: without it, a Create that started
	// clearing the marker would leave this test asserting !IsDeferred against
	// a bead that was never deferred, and it would pass vacuously.
	primed, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before reopen: %v", err)
	}
	if !IsDeferred(primed, time.Now()) {
		t.Fatalf("fixture is not deferred before the reopen, so the assertion below would be vacuous: %+v", primed)
	}

	backing.failNextGet = true
	if err := cache.Reopen(created.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	reopened, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if IsDeferred(reopened, time.Now()) {
		t.Fatalf("failed refresh fallback retained status-based deferral: %+v", reopened)
	}
}

func TestCachingStoreCloseKeepsCachedSynthesisWhenRefreshFails(t *testing.T) {
	t.Parallel()

	// When the post-close refresh Get fails, Close must still fall back to the
	// cached-status synthesis (today's behavior) rather than dropping the entry.
	backing := &releaseRefreshFailOnceStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "close-refresh-fails"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	backing.failNextGet = true
	if err := cache.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The synthesis keeps the entry cached (distinguishing it from an evict,
	// whose follow-up Get would fall through to the backing and also report
	// closed).
	cache.mu.RLock()
	_, inBeads := cache.beads[b.ID]
	cache.mu.RUnlock()
	if !inBeads {
		t.Fatal("entry missing from the cache after Close with failed refresh; want the cached-status synthesis, not an evict")
	}

	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("cache Get after close with failed refresh: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("cached status after Close with failed refresh = %q, want %q (synthesis fallback)", got.Status, "closed")
	}
}

// TestCachingStoreWriteMatchingStaleCacheReachesBacking pins that a write the
// cached row already reflects still reaches the backing store. The backing
// moves underneath a primed, clean cache that never sees the change; the write
// asks for the value the cache still holds, and the backing must end up with
// it. A cache that skipped the write because its possibly-stale row matched
// would leave the other writer's value in place and report success.
func TestCachingStoreWriteMatchingStaleCacheReachesBacking(t *testing.T) {
	t.Parallel()

	strPtr := func(s string) *string { return &s }
	intPtr := func(i int) *int { return &i }
	cases := []struct {
		name  string
		seed  Bead
		drift UpdateOpts // the write that lands around the cache
		write func(c *CachingStore, id string) error
		calls func(b *countingBackingStore) int
		check func(b Bead) bool
	}{
		{
			name: "SetMetadata", seed: Bead{Title: "x", Metadata: map[string]string{"k": "v"}},
			drift: UpdateOpts{Metadata: map[string]string{"k": "other"}},
			write: func(c *CachingStore, id string) error { return c.SetMetadata(id, "k", "v") },
			calls: func(b *countingBackingStore) int { return b.setMetadataCalls },
			check: func(b Bead) bool { return b.Metadata["k"] == "v" },
		},
		{
			name: "SetMetadataBatch", seed: Bead{Title: "x", Metadata: map[string]string{"k": "v", "j": "w"}},
			drift: UpdateOpts{Metadata: map[string]string{"k": "other"}},
			write: func(c *CachingStore, id string) error {
				return c.SetMetadataBatch(id, map[string]string{"k": "v", "j": "w"})
			},
			calls: func(b *countingBackingStore) int { return b.setMetadataBatchCalls },
			check: func(b Bead) bool { return b.Metadata["k"] == "v" && b.Metadata["j"] == "w" },
		},
		{
			name: "Update/Title", seed: Bead{Title: "pinned"},
			drift: UpdateOpts{Title: strPtr("other")},
			write: func(c *CachingStore, id string) error { return c.Update(id, UpdateOpts{Title: strPtr("pinned")}) },
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.Title == "pinned" },
		},
		{
			name: "Update/Status", seed: Bead{Title: "x", Status: "open"},
			drift: UpdateOpts{Status: strPtr("in_progress")},
			write: func(c *CachingStore, id string) error { return c.Update(id, UpdateOpts{Status: strPtr("open")}) },
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.Status == "open" },
		},
		{
			name: "Update/Type", seed: Bead{Title: "x", Type: "task"},
			drift: UpdateOpts{Type: strPtr("bug")},
			write: func(c *CachingStore, id string) error { return c.Update(id, UpdateOpts{Type: strPtr("task")}) },
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.Type == "task" },
		},
		{
			name: "Update/Priority", seed: Bead{Title: "x", Priority: intPtr(2)},
			drift: UpdateOpts{Priority: intPtr(3)},
			write: func(c *CachingStore, id string) error { return c.Update(id, UpdateOpts{Priority: intPtr(2)}) },
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.Priority != nil && *b.Priority == 2 },
		},
		{
			name: "Update/Description", seed: Bead{Title: "x", Description: "body"},
			drift: UpdateOpts{Description: strPtr("other")},
			write: func(c *CachingStore, id string) error {
				return c.Update(id, UpdateOpts{Description: strPtr("body")})
			},
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.Description == "body" },
		},
		{
			name: "Update/ParentID", seed: Bead{Title: "x", ParentID: "gc-parent"},
			drift: UpdateOpts{ParentID: strPtr("gc-other")},
			write: func(c *CachingStore, id string) error {
				return c.Update(id, UpdateOpts{ParentID: strPtr("gc-parent")})
			},
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.ParentID == "gc-parent" },
		},
		{
			name: "Update/Assignee", seed: Bead{Title: "x", Assignee: "alice"},
			drift: UpdateOpts{Assignee: strPtr("bob")},
			write: func(c *CachingStore, id string) error { return c.Update(id, UpdateOpts{Assignee: strPtr("alice")}) },
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.Assignee == "alice" },
		},
		{
			name: "Update/Metadata", seed: Bead{Title: "x", Metadata: map[string]string{"k": "v"}},
			drift: UpdateOpts{Metadata: map[string]string{"k": "other"}},
			write: func(c *CachingStore, id string) error {
				return c.Update(id, UpdateOpts{Metadata: map[string]string{"k": "v"}})
			},
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return b.Metadata["k"] == "v" },
		},
		{
			name: "Update/Labels", seed: Bead{Title: "x", Labels: []string{"a"}},
			drift: UpdateOpts{RemoveLabels: []string{"a"}},
			write: func(c *CachingStore, id string) error { return c.Update(id, UpdateOpts{Labels: []string{"a"}}) },
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return slices.Contains(b.Labels, "a") },
		},
		{
			name: "Update/RemoveLabels", seed: Bead{Title: "x"},
			drift: UpdateOpts{Labels: []string{"z"}},
			write: func(c *CachingStore, id string) error {
				return c.Update(id, UpdateOpts{RemoveLabels: []string{"z"}})
			},
			calls: func(b *countingBackingStore) int { return b.updateCalls },
			check: func(b Bead) bool { return !slices.Contains(b.Labels, "z") },
		},
	}
	// The SQLite leg is a store that skips same-value writes itself: the
	// write must still reach it, and it must find the row changed.
	backings := []struct {
		name string
		open func(t *testing.T) Store
	}{
		{"MemStore", func(*testing.T) Store { return NewMemStore() }},
		{"SQLiteStore", func(t *testing.T) Store {
			s, err := OpenSQLiteStore(t.TempDir())
			if err != nil {
				t.Fatalf("OpenSQLiteStore: %v", err)
			}
			return s
		}},
	}
	for _, store := range backings {
		for _, tc := range cases {
			t.Run(store.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				backing := &countingBackingStore{Store: store.open(t)}
				bead, err := backing.Create(tc.seed)
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				cache := NewCachingStoreForTest(backing, nil)
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatalf("Prime: %v", err)
				}
				if err := backing.Store.Update(bead.ID, tc.drift); err != nil {
					t.Fatalf("drift Update: %v", err)
				}
				before := tc.calls(backing)

				if err := tc.write(cache, bead.ID); err != nil {
					t.Fatalf("write: %v", err)
				}
				if got := tc.calls(backing) - before; got != 1 {
					t.Errorf("backing write calls = %d, want 1: a stale cached match must not skip the write", got)
				}
				got, err := backing.Get(bead.ID)
				if err != nil {
					t.Fatalf("backing Get: %v", err)
				}
				if !tc.check(got) {
					t.Errorf("backing row %+v does not carry the write; the stale cache turned it into a no-op", got)
				}
			})
		}
	}
}

// TestCachingStoreCloseOnStaleClosedRowReachesBacking is the Close half of
// TestCachingStoreWriteMatchingStaleCacheReachesBacking: the cache holds the
// row closed while the backing was reopened around it, and the close must
// still reach the backing.
func TestCachingStoreCloseOnStaleClosedRowReachesBacking(t *testing.T) {
	t.Parallel()

	backing := &countingBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "x"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Close(bead.ID); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := backing.Reopen(bead.ID); err != nil {
		t.Fatalf("backing Reopen: %v", err)
	}
	backing.closeCalls = 0

	if err := cache.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if backing.closeCalls != 1 {
		t.Errorf("backing.Close called %d times, want 1: a stale closed row must not skip the close", backing.closeCalls)
	}
	got, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("backing status = %q, want closed", got.Status)
	}
}
