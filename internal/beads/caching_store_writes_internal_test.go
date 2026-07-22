package beads

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
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

type latchedLegacyCloseStore struct {
	Store
	blockedID string
	entered   chan struct{}
	release   chan struct{}
}

func (s *latchedLegacyCloseStore) Close(id string) error {
	if id == s.blockedID {
		close(s.entered)
		<-s.release
	}
	return s.Store.Close(id)
}

type latchedCommittedCloseStore struct {
	Store
	committed chan struct{}
	release   chan struct{}
}

type latchedLegacyCloseReopenStore struct {
	*latchedCommittedCloseStore
	reopenEntered chan struct{}
}

func (s *latchedLegacyCloseReopenStore) Reopen(id string) error {
	close(s.reopenEntered)
	return s.Store.Reopen(id)
}

func (s *latchedCommittedCloseStore) Close(id string) error {
	err := s.Store.Close(id)
	close(s.committed)
	<-s.release
	return err
}

func (s *latchedCommittedCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := s.Store.CloseAll(ids, metadata)
	close(s.committed)
	<-s.release
	return n, err
}

func (s *latchedCommittedCloseStore) CloseAllWithTransitions(ids []string, metadata map[string]string) (CloseAllTransitionResult, error) {
	transitioner, ok := CloseAllTransitionerFor(s.Store)
	if !ok {
		return CloseAllTransitionResult{}, ErrCloseAllTransitionUnsupported
	}
	result, err := transitioner.CloseAllWithTransitions(ids, metadata)
	close(s.committed)
	<-s.release
	return result, err
}

// reconcileAcrossPausedMutation starts reconciliation while a backing mutation
// is paused after commit. Reconciliation may finish first on an implementation
// that does not serialize the two; otherwise release the mutation once the
// unknown-scope reconciliation is observably queued, then await its completion.
func reconcileAcrossPausedMutation(t *testing.T, cache *CachingStore, release chan struct{}) {
	t.Helper()
	reconcileDone := make(chan struct{})
	go func() {
		cache.runReconciliation()
		close(reconcileDone)
	}()

	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	for {
		select {
		case <-reconcileDone:
			close(release)
			return
		default:
		}

		cache.closeStateMu.Lock()
		waiters := cache.unknownMutationWaiters
		cache.closeStateMu.Unlock()
		if waiters > 0 {
			close(release)
			select {
			case <-reconcileDone:
				return
			case <-timer.C:
				t.Fatal("reconciliation did not complete after paused mutation released")
			}
		}

		select {
		case <-timer.C:
			t.Fatal("reconciliation neither completed nor queued on mutation scope")
		default:
			runtime.Gosched()
		}
	}
}

type latchedAtomicCloseReopenStore struct {
	Store
	closeCommitted chan struct{}
	releaseClose   chan struct{}
	reopenEntered  chan struct{}
}

type commitThenErrorAtomicCloseStore struct {
	Store
	closeErr       error
	getCalls       int
	stripOwnership bool
}

type commitThenErrorLegacyCloseStore struct {
	Store
	closeErr error
	getCalls int
}

func (s *commitThenErrorLegacyCloseStore) Get(id string) (Bead, error) {
	s.getCalls++
	return s.Store.Get(id)
}

func (s *commitThenErrorLegacyCloseStore) Close(id string) error {
	if err := s.Store.Close(id); err != nil {
		return err
	}
	return s.closeErr
}

type commitThenZeroErrorCloseAllStore struct {
	Store
	closeErr error
	getCalls int
}

func (s *commitThenZeroErrorCloseAllStore) Get(id string) (Bead, error) {
	s.getCalls++
	return s.Store.Get(id)
}

func (s *commitThenZeroErrorCloseAllStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if _, err := s.Store.CloseAll(ids, metadata); err != nil {
		return 0, err
	}
	return 0, s.closeErr
}

func (s *commitThenErrorAtomicCloseStore) Get(id string) (Bead, error) {
	s.getCalls++
	return s.Store.Get(id)
}

func (s *commitThenErrorAtomicCloseStore) CloseWithReasonIfOpen(id, reason string) (CloseTransition, error) {
	closer, ok := CloseTransitionerFor(s.Store)
	if !ok {
		return CloseTransition{}, ErrCloseTransitionUnsupported
	}
	transition, err := closer.CloseWithReasonIfOpen(id, reason)
	if err != nil {
		return CloseTransition{}, err
	}
	if s.stripOwnership {
		transition.Transitioned = false
	}
	return transition, s.closeErr
}

func (s *latchedAtomicCloseReopenStore) CloseWithReasonIfOpen(id, reason string) (CloseTransition, error) {
	closer, ok := CloseTransitionerFor(s.Store)
	if !ok {
		return CloseTransition{}, ErrCloseTransitionUnsupported
	}
	transition, err := closer.CloseWithReasonIfOpen(id, reason)
	close(s.closeCommitted)
	<-s.releaseClose
	return transition, err
}

func (s *latchedAtomicCloseReopenStore) Reopen(id string) error {
	close(s.reopenEntered)
	return s.Store.Reopen(id)
}

func waitForCacheMutationWaiter(t *testing.T, cache *CachingStore, id string) {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	for {
		cache.closeStateMu.Lock()
		entry := cache.closeStateLocks[id]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		cache.closeStateMu.Unlock()
		if refs >= 2 {
			return
		}
		select {
		case <-timer.C:
			t.Fatal("mutation did not reach the in-flight per-ID serialization point")
		default:
			runtime.Gosched()
		}
	}
}

type releaseRefreshFailOnceStore struct {
	Store
	failNextGet bool
	getCalls    int
}

type closeIfMatchRefreshGateStore struct {
	*MemStore
	gateRefresh   atomic.Bool
	closeReturned chan struct{}
	refreshRead   chan struct{}
	allowRefresh  chan struct{}
}

func (s *closeIfMatchRefreshGateStore) CloseIfMatch(id string, expectedRevision int64) error {
	if err := s.MemStore.CloseIfMatch(id, expectedRevision); err != nil {
		return err
	}
	s.gateRefresh.Store(true)
	close(s.closeReturned)
	return nil
}

func (s *closeIfMatchRefreshGateStore) Get(id string) (Bead, error) {
	if s.gateRefresh.CompareAndSwap(true, false) {
		close(s.refreshRead)
		<-s.allowRefresh
	}
	return s.MemStore.Get(id)
}

func (s *releaseRefreshFailOnceStore) Get(id string) (Bead, error) {
	s.getCalls++
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

func TestCachingStoreCloseKeepsFailedRefreshDirtyUntilBackingConverges(t *testing.T) {
	t.Parallel()

	// This wrapper intentionally exposes only Store, not CloseTransitioner, so
	// the legacy close+refresh fallback remains covered. A locally synthesized
	// closed row has no authoritative post-close revision and must stay dirty.
	backing := &releaseRefreshFailOnceStore{Store: NewMemStore()}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	b, err := cache.Create(Bead{Title: "close-refresh-fails"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	notes = nil

	backing.failNextGet = true
	if err := cache.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cache.mu.RLock()
	_, inBeads := cache.beads[b.ID]
	_, dirty := cache.dirty[b.ID]
	cache.mu.RUnlock()
	if !inBeads {
		t.Fatal("entry missing after Close with failed refresh; want a dirty synthesis until the next read")
	}
	if !dirty {
		t.Fatal("failed-refresh synthesis is clean; the next Get could serve a fabricated post-close revision")
	}
	if len(notes) != 0 {
		t.Fatalf("failed-refresh close notifications = %+v, want no fabricated snapshot", notes)
	}

	getCallsBefore := backing.getCalls
	got, err := cache.Get(b.ID)
	if err != nil {
		t.Fatalf("cache Get after close with failed refresh: %v", err)
	}
	if backing.getCalls != getCallsBefore+1 {
		t.Fatalf("backing Get calls = %d, want %d; dirty entry must re-read backing", backing.getCalls, getCallsBefore+1)
	}
	if got.Status != "closed" {
		t.Fatalf("status after convergence = %q, want closed", got.Status)
	}
	cache.mu.RLock()
	_, dirty = cache.dirty[b.ID]
	cache.mu.RUnlock()
	if dirty {
		t.Fatal("entry remains dirty after an authoritative backing read")
	}
}

func TestCachingStoreFailedPostWriteRefreshNeverPublishesPatchedSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, *MemStore, string)
		mutate func(*CachingStore, *releaseRefreshFailOnceStore, string) error
		assert func(*testing.T, Bead)
	}{
		{
			name: "Reopen",
			setup: func(t *testing.T, store *MemStore, id string) {
				t.Helper()
				if err := store.Close(id); err != nil {
					t.Fatalf("setup close: %v", err)
				}
			},
			mutate: func(cache *CachingStore, _ *releaseRefreshFailOnceStore, id string) error {
				return cache.Reopen(id)
			},
			assert: func(t *testing.T, bead Bead) {
				t.Helper()
				if bead.Status != "open" {
					t.Fatalf("durable status = %q, want open", bead.Status)
				}
			},
		},
		{
			name: "SetMetadata",
			mutate: func(cache *CachingStore, _ *releaseRefreshFailOnceStore, id string) error {
				return cache.SetMetadata(id, "written", "single")
			},
			assert: func(t *testing.T, bead Bead) {
				t.Helper()
				if bead.Metadata["written"] != "single" {
					t.Fatalf("durable metadata = %+v, want single write", bead.Metadata)
				}
			},
		},
		{
			name: "SetMetadataBatch",
			mutate: func(cache *CachingStore, _ *releaseRefreshFailOnceStore, id string) error {
				return cache.SetMetadataBatch(id, map[string]string{"written": "batch"})
			},
			assert: func(t *testing.T, bead Bead) {
				t.Helper()
				if bead.Metadata["written"] != "batch" {
					t.Fatalf("durable metadata = %+v, want batch write", bead.Metadata)
				}
			},
		},
		{
			name: "TxClose",
			mutate: func(cache *CachingStore, backing *releaseRefreshFailOnceStore, id string) error {
				return cache.Tx("close before failed refresh", func(tx Tx) error {
					if err := tx.Close(id); err != nil {
						return err
					}
					backing.failNextGet = true
					return nil
				})
			},
			assert: func(t *testing.T, bead Bead) {
				t.Helper()
				if bead.Status != "closed" {
					t.Fatalf("durable status = %q, want closed", bead.Status)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := NewMemStore()
			created, err := base.Create(Bead{
				Title:       "authoritative refresh only",
				Description: "must survive",
				Metadata:    StringMap{"existing": "kept"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if tt.setup != nil {
				tt.setup(t, base, created.ID)
			}
			backing := &releaseRefreshFailOnceStore{Store: base}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if tt.name != "TxClose" {
				backing.failNextGet = true
			}
			if err := tt.mutate(cache, backing, created.ID); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			if len(notes) != 0 {
				t.Fatalf("failed-refresh notifications = %+v, want none", notes)
			}
			cache.mu.RLock()
			_, dirty := cache.dirty[created.ID]
			cache.mu.RUnlock()
			if !dirty {
				t.Fatal("failed post-write refresh left a clean cached row")
			}
			fresh, err := cache.Get(created.ID)
			if err != nil {
				t.Fatalf("authoritative Get: %v", err)
			}
			if fresh.Description != "must survive" || fresh.Metadata["existing"] != "kept" {
				t.Fatalf("authoritative row lost unrelated fields: %+v", fresh)
			}
			tt.assert(t, fresh)
		})
	}
}

func TestCachingStoreCloseIfMatchDoesNotRewriteRacingReopenAsClosed(t *testing.T) {
	backing := &closeIfMatchRefreshGateStore{
		MemStore:      NewMemStore(),
		closeReturned: make(chan struct{}),
		refreshRead:   make(chan struct{}),
		allowRefresh:  make(chan struct{}),
	}
	created, err := backing.Create(Bead{Title: "conditional close race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- cache.CloseIfMatch(created.ID, created.Revision) }()
	select {
	case <-backing.closeReturned:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("conditional close did not commit")
	}
	select {
	case <-backing.refreshRead:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("conditional close refresh did not pause")
	}
	if err := backing.Reopen(created.ID); err != nil {
		t.Fatalf("racing Reopen: %v", err)
	}
	close(backing.allowRefresh)
	if err := <-closeDone; err != nil {
		t.Fatalf("CloseIfMatch: %v", err)
	}

	for _, note := range notes {
		payload, ok := DecodeBeadEventPayload(note.payload)
		if note.eventType == "bead.closed" || ok && payload.Status == "closed" {
			t.Fatalf("racing reopen was rewritten as closed: notification=%+v payload=%+v", note, payload)
		}
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" {
		t.Fatalf("notifications = %+v, want one exact bead.updated observation", notes)
	}
	observed, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || observed.Status != "open" {
		t.Fatalf("updated payload = %#v ok=%v, want exact reopened row", observed, ok)
	}
	durable, err := backing.MemStore.Get(created.ID)
	if err != nil {
		t.Fatalf("durable Get: %v", err)
	}
	if durable.Status != "open" {
		t.Fatalf("durable status = %q, want open", durable.Status)
	}
}

func TestCachingStoreReopenRestoresDependenciesDroppedByClose(t *testing.T) {
	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "open blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	target, err := backing.Create(Bead{Title: "blocked target", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create target: %v", err)
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, err := cache.CloseAll([]string{target.ID}, nil); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	notes = nil

	if err := cache.Reopen(target.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" {
		t.Fatalf("notifications = %+v, want one bead.updated", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || len(payload.Dependencies) != 1 || payload.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("updated payload = %#v ok=%v, want restored blocker %s", payload, ok, blocker.ID)
	}
	ready, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable after Reopen")
	}
	for _, bead := range ready {
		if bead.ID == target.ID {
			t.Fatalf("reopened target %s became ready despite open blocker %s", target.ID, blocker.ID)
		}
	}
}

func TestCachingStoreClosePreservesStampedReasonThroughAtomicBacking(t *testing.T) {
	const reason = "reason stamped before ordinary close"
	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "close payload blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	created, err := backing.Create(Bead{
		Title:    "pre-stamped close",
		Metadata: StringMap{"close_reason": reason},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})

	if err := cache.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	durable, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := durable.Metadata["close_reason"]; got != reason {
		t.Fatalf("durable close_reason = %q, want %q", got, reason)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" {
		t.Fatalf("notifications = %+v, want one bead.closed", notes)
	}
	decoded, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok {
		t.Fatalf("decode notification payload: %s", notes[0].payload)
	}
	if got := decoded.Metadata["close_reason"]; got != reason {
		t.Fatalf("notification close_reason = %q, want %q", got, reason)
	}
	if len(decoded.Dependencies) != 1 || decoded.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("notification dependencies = %#v, want blocker %s", decoded.Dependencies, blocker.ID)
	}
}

func TestCachingStoreCloseNotifiesOnceWhenCacheIsNotLive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state cacheState
	}{{"uninitialized", cacheUninitialized}, {"degraded", cacheDegraded}} {
		t.Run(tc.name, func(t *testing.T) {
			backing := NewMemStore()
			created, err := backing.Create(Bead{Title: "close once"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			cache.mu.Lock()
			cache.state = tc.state
			cache.mu.Unlock()

			if err := cache.Close(created.ID); err != nil {
				t.Fatalf("first Close: %v", err)
			}
			if err := cache.Close(created.ID); err != nil {
				t.Fatalf("second Close: %v", err)
			}

			closed := 0
			for _, note := range notes {
				if note.eventType == "bead.closed" && note.beadID == created.ID {
					closed++
				}
			}
			if closed != 1 {
				t.Fatalf("bead.closed notifications = %d, want exactly 1: %+v", closed, notes)
			}
		})
	}
}

func TestCachingStoreCloseWithoutObserverDoesNotMuteConcurrentClose(t *testing.T) {
	base := NewMemStore()
	silent, err := base.Create(Bead{Title: "unprovable autoclose"})
	if err != nil {
		t.Fatalf("Create(silent): %v", err)
	}
	loud, err := base.Create(Bead{Title: "unrelated user close"})
	if err != nil {
		t.Fatalf("Create(loud): %v", err)
	}
	backing := &latchedLegacyCloseStore{
		Store:     base,
		blockedID: silent.ID,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})

	silentDone := make(chan error, 1)
	go func() { silentDone <- cache.CloseWithoutObserver(silent.ID) }()
	select {
	case <-backing.entered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("observer-suppressed close did not reach backing store")
	}

	if err := cache.Close(loud.ID); err != nil {
		t.Fatalf("concurrent Close(loud): %v", err)
	}
	close(backing.release)
	select {
	case err := <-silentDone:
		if err != nil {
			t.Fatalf("CloseWithoutObserver(silent): %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("observer-suppressed close did not complete")
	}

	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != loud.ID {
		t.Fatalf("notifications = %+v, want only the committed legacy close edge for %s", notes, loud.ID)
	}
}

func TestCachingStoreCloseWithoutObserverSuppressesConcurrentReconcile(t *testing.T) {
	tests := []struct {
		name  string
		close func(*CachingStore, string) error
	}{
		{
			name: "single close",
			close: func(cache *CachingStore, id string) error {
				return cache.CloseWithoutObserver(id)
			},
		},
		{
			name: "batch close",
			close: func(cache *CachingStore, id string) error {
				_, err := cache.CloseAllWithoutObserver([]string{id}, map[string]string{"close_reason": "sidecar cleanup"})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := NewMemStore()
			created, err := base.Create(Bead{Title: "suppressed close target"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			backing := &latchedCommittedCloseStore{
				Store:     base,
				committed: make(chan struct{}),
				release:   make(chan struct{}),
			}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- tc.close(cache, created.ID) }()
			select {
			case <-backing.committed:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("observer-suppressed close did not commit in backing store")
			}

			reconcileAcrossPausedMutation(t, cache, backing.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("observer-suppressed close: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("observer-suppressed close did not complete")
			}

			for _, note := range notes {
				if note.eventType == "bead.closed" && note.beadID == created.ID {
					t.Fatalf("reconciliation emitted suppressed close notification: %+v", notes)
				}
			}
		})
	}
}

func TestCachingStoreCloseEmitsOnceAcrossConcurrentReconcile(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		close     func(*CachingStore, string) error
	}{
		{
			// A legacy Close is a committed close (bead.closed); the concurrent
			// reconcile must not synthesize a SECOND bead.closed when it evicts the
			// already-closed cache row (len(notes)==1 proves the edge fires once).
			name:      "single close",
			eventType: "bead.closed",
			close: func(cache *CachingStore, id string) error {
				return cache.Close(id)
			},
		},
		{
			name:      "batch close",
			eventType: "bead.closed",
			close: func(cache *CachingStore, id string) error {
				_, err := cache.CloseAll([]string{id}, map[string]string{"close_reason": "batch close"})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := NewMemStore()
			created, err := base.Create(Bead{Title: "close target"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			backing := &latchedCommittedCloseStore{
				Store:     base,
				committed: make(chan struct{}),
				release:   make(chan struct{}),
			}
			var notes []cacheWriteNotification
			cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
				notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
			})
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- tc.close(cache, created.ID) }()
			select {
			case <-backing.committed:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("close did not commit in backing store")
			}

			reconcileAcrossPausedMutation(t, cache, backing.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("close: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("close did not complete")
			}

			matched := 0
			for _, note := range notes {
				if note.eventType == tc.eventType && note.beadID == created.ID {
					matched++
				}
			}
			if matched != 1 || len(notes) != 1 {
				t.Fatalf("%s notifications = %d, want exactly 1: %+v", tc.eventType, matched, notes)
			}
		})
	}
}

func TestCachingStoreAtomicCloseEmitsOnceAcrossConcurrentReconcile(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "atomic close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &latchedAtomicCloseReopenStore{
		Store:          base,
		closeCommitted: make(chan struct{}),
		releaseClose:   make(chan struct{}),
		reopenEntered:  make(chan struct{}),
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := cache.CloseWithReasonIfOpen(created.ID, "atomic close")
		done <- err
	}()
	select {
	case <-backing.closeCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("atomic close did not commit in backing store")
	}

	reconcileAcrossPausedMutation(t, cache, backing.releaseClose)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CloseWithReasonIfOpen: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("atomic close did not complete")
	}

	closed := 0
	for _, note := range notes {
		if note.eventType == "bead.closed" && note.beadID == created.ID {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("bead.closed notifications = %d, want exactly 1: %+v", closed, notes)
	}
}

func TestCachingStoreAtomicCloseCommittedErrorPublishesAuthoritativeTransition(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "ambiguous atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ambiguousErr := errors.New("connection reset after atomic close")
	backing := &commitThenErrorAtomicCloseStore{Store: base, closeErr: ambiguousErr}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if _, err := cache.CloseWithReasonIfOpen(created.ID, "committed before disconnect"); !errors.Is(err, ambiguousErr) {
		t.Fatalf("CloseWithReasonIfOpen error = %v, want %v", err, ambiguousErr)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one authoritative bead.closed result", notes)
	}

	getCallsBefore := backing.getCalls
	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after ambiguous close: %v", err)
	}
	if backing.getCalls != getCallsBefore {
		t.Fatalf("backing Get calls = %d, want %d; authoritative transition should refresh the cache", backing.getCalls, getCallsBefore)
	}
	if got.Status != "closed" {
		t.Fatalf("status after backing read = %q, want closed", got.Status)
	}
	if len(notes) != 1 {
		t.Fatalf("notifications after backing read = %+v, want no duplicate after the authoritative close", notes)
	}
}

func TestCachingStoreAtomicCloseAmbiguousCommitPublishesConservativeUpdate(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "ambiguous atomic close without ownership"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ambiguousErr := errors.New("connection reset after atomic close")
	backing := &commitThenErrorAtomicCloseStore{
		Store:          base,
		closeErr:       ambiguousErr,
		stripOwnership: true,
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if _, err := cache.CloseWithReasonIfOpen(created.ID, "committed before disconnect"); !errors.Is(err, ambiguousErr) {
		t.Fatalf("CloseWithReasonIfOpen error = %v, want %v", err, ambiguousErr)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one conservative bead.updated for the changed authoritative snapshot", notes)
	}
	closed, _, err := decodeCacheEvent(notes[0].payload)
	if err != nil {
		t.Fatalf("decode notification: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("bead.updated status = %q, want closed", closed.Status)
	}
}

func TestCachingStoreSuppressedAmbiguousCloseReturnsBarrierWithoutNotification(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "suppressed ambiguous atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ambiguousErr := errors.New("connection reset after atomic close")
	backing := &commitThenErrorAtomicCloseStore{
		Store:          base,
		closeErr:       ambiguousErr,
		stripOwnership: true,
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	delivery, err := cache.CloseWithoutObserverWithDelivery(created.ID)
	if !errors.Is(err, ambiguousErr) {
		t.Fatalf("CloseWithoutObserverWithDelivery error = %v, want %v", err, ambiguousErr)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want observer-suppressed close to remain silent", notes)
	}
	if delivery == nil {
		t.Fatal("CloseWithoutObserverWithDelivery delivery = nil, want ordering barrier")
	}
	delivered := false
	delivery.AfterDelivery(func() { delivered = true })
	if !delivered {
		t.Fatal("suppressed ambiguous close barrier was not delivered")
	}
}

func TestCachingStoreLegacyCloseAmbiguousErrorDirtiesAndRefencesTarget(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "ambiguous legacy close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ambiguousErr := errors.New("connection reset after legacy close")
	backing := &commitThenErrorLegacyCloseStore{Store: base, closeErr: ambiguousErr}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.mu.RLock()
	seqBefore := cache.mutationSeq
	cache.mu.RUnlock()
	if err := cache.Close(created.ID); !errors.Is(err, ambiguousErr) {
		t.Fatalf("Close error = %v, want %v", err, ambiguousErr)
	}

	cache.mu.RLock()
	seqAfter := cache.beadSeq[created.ID]
	_, dirty := cache.dirty[created.ID]
	cache.mu.RUnlock()
	if seqAfter < seqBefore+2 {
		t.Fatalf("beadSeq after ambiguous close = %d, want at least %d (entry fence plus error fence)", seqAfter, seqBefore+2)
	}
	if !dirty {
		t.Fatal("ambiguous legacy close left the cached row clean")
	}

	getCallsBefore := backing.getCalls
	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after ambiguous close: %v", err)
	}
	if backing.getCalls != getCallsBefore+1 {
		t.Fatalf("backing Get calls = %d, want %d", backing.getCalls, getCallsBefore+1)
	}
	if got.Status != "closed" {
		t.Fatalf("status after backing read = %q, want closed", got.Status)
	}
}

func TestCachingStoreCloseAllZeroCountAmbiguousErrorDirtiesAndRefencesEveryTarget(t *testing.T) {
	base := NewMemStore()
	first, err := base.Create(Bead{Title: "first ambiguous batch close"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := base.Create(Bead{Title: "second ambiguous batch close"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	ambiguousErr := errors.New("connection reset after batch close")
	backing := &commitThenZeroErrorCloseAllStore{Store: base, closeErr: ambiguousErr}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.mu.RLock()
	seqBefore := cache.mutationSeq
	cache.mu.RUnlock()
	ids := []string{first.ID, second.ID}
	closed, err := cache.CloseAll(ids, map[string]string{"close_reason": "ambiguous batch"})
	if !errors.Is(err, ambiguousErr) {
		t.Fatalf("CloseAll error = %v, want %v", err, ambiguousErr)
	}
	if closed != 0 {
		t.Fatalf("CloseAll closed = %d, want reported 0", closed)
	}

	cache.mu.RLock()
	for _, id := range ids {
		if seq := cache.beadSeq[id]; seq < seqBefore+2 {
			cache.mu.RUnlock()
			t.Fatalf("beadSeq[%s] after ambiguous batch = %d, want at least %d", id, seq, seqBefore+2)
		}
		if _, dirty := cache.dirty[id]; !dirty {
			cache.mu.RUnlock()
			t.Fatalf("ambiguous batch left %s clean", id)
		}
	}
	cache.mu.RUnlock()

	getCallsBefore := backing.getCalls
	for _, id := range ids {
		got, getErr := cache.Get(id)
		if getErr != nil {
			t.Fatalf("Get(%s) after ambiguous batch: %v", id, getErr)
		}
		if got.Status != "closed" {
			t.Fatalf("Get(%s) status = %q, want closed", id, got.Status)
		}
	}
	if backing.getCalls != getCallsBefore+len(ids) {
		t.Fatalf("backing Get calls = %d, want %d", backing.getCalls, getCallsBefore+len(ids))
	}
}

func TestCachingStoreBatchCloseEmitsAfterSuppressedReconcileEviction(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "long batch close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &latchedCommittedCloseStore{
		Store:     base,
		committed: make(chan struct{}),
		release:   make(chan struct{}),
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := cache.CloseAll([]string{created.ID}, map[string]string{"close_reason": "long batch close"})
		done <- err
	}()
	select {
	case <-backing.committed:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("batch close did not commit in backing store")
	}

	// Model an in-flight close older than recentLocalMutation's five-second
	// window without adding a wall-clock sleep to the test.
	cache.mu.Lock()
	cache.localBeadAt[created.ID] = time.Now().Add(-6 * time.Second)
	cache.mu.Unlock()
	reconcileAcrossPausedMutation(t, cache, backing.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CloseAll: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("batch close did not complete")
	}

	closed := 0
	for _, note := range notes {
		if note.eventType == "bead.closed" && note.beadID == created.ID {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("bead.closed notifications = %d, want exactly 1 after suppressed eviction: %+v", closed, notes)
	}
}

func TestCachingStoreCloseWithoutObserverRejectsStaleConcurrentCreatedSnapshot(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "cold close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &latchedLegacyCloseStore{
		Store:     base,
		blockedID: created.ID,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})

	done := make(chan error, 1)
	go func() { done <- cache.CloseWithoutObserver(created.ID) }()
	select {
	case <-backing.entered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("observer-suppressed close did not reach backing store")
	}

	reconcileAcrossPausedMutation(t, cache, backing.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CloseWithoutObserver: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("observer-suppressed close did not complete")
	}

	// The close wins the mutation scope before reconciliation can merge its
	// older open-row snapshot. The close's sequence fence must reject that
	// snapshot rather than publish bead.created after the durable close. This
	// operation suppresses its own bead.closed callback, so no cache observer
	// notification is expected.
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none from the stale pre-close snapshot", notes)
	}
}

func TestCachingStoreCloseCannotOverwriteConcurrentReopen(t *testing.T) {
	tests := []struct {
		name       string
		newBacking func(Store) (Store, chan struct{}, chan struct{}, chan struct{})
		close      func(*CachingStore, string) error
	}{
		{
			name: "legacy close",
			newBacking: func(base Store) (Store, chan struct{}, chan struct{}, chan struct{}) {
				committed := make(chan struct{})
				release := make(chan struct{})
				reopenEntered := make(chan struct{})
				return &latchedLegacyCloseReopenStore{
					latchedCommittedCloseStore: &latchedCommittedCloseStore{
						Store:     base,
						committed: committed,
						release:   release,
					},
					reopenEntered: reopenEntered,
				}, committed, release, reopenEntered
			},
			close: func(cache *CachingStore, id string) error {
				return cache.Close(id)
			},
		},
		{
			name: "atomic close",
			newBacking: func(base Store) (Store, chan struct{}, chan struct{}, chan struct{}) {
				committed := make(chan struct{})
				release := make(chan struct{})
				reopenEntered := make(chan struct{})
				return &latchedAtomicCloseReopenStore{
					Store:          base,
					closeCommitted: committed,
					releaseClose:   release,
					reopenEntered:  reopenEntered,
				}, committed, release, reopenEntered
			},
			close: func(cache *CachingStore, id string) error {
				_, err := cache.CloseWithReasonIfOpen(id, "racing close")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := NewMemStore()
			created, err := base.Create(Bead{Title: "close-reopen race"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			backing, closeCommitted, releaseClose, reopenEntered := tc.newBacking(base)
			cache := NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			closeDone := make(chan error, 1)
			go func() { closeDone <- tc.close(cache, created.ID) }()
			select {
			case <-closeCommitted:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("close did not commit in backing store")
			}

			reopenDone := make(chan error, 1)
			go func() { reopenDone <- cache.Reopen(created.ID) }()
			waitForCacheMutationWaiter(t, cache, created.ID)
			select {
			case <-reopenEntered:
				t.Fatal("reopen reached backing store before close finalization")
			default:
			}
			close(releaseClose)

			select {
			case err := <-closeDone:
				if err != nil {
					t.Fatalf("close: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("close did not complete")
			}
			select {
			case err := <-reopenDone:
				if err != nil {
					t.Fatalf("Reopen: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("reopen did not complete")
			}

			for name, store := range map[string]Store{"cache": cache, "backing": base} {
				got, err := store.Get(created.ID)
				if err != nil {
					t.Fatalf("Get(%s): %v", name, err)
				}
				if got.Status != "open" {
					t.Fatalf("%s status = %q, want durable reopened status", name, got.Status)
				}
			}
		})
	}
}

func TestCachingStoreCloseReopenNotificationsFollowDurableOrder(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "notification order race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closedCallbackEntered := make(chan struct{})
	releaseClosedCallback := make(chan struct{})
	var notes []string
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, _ json.RawMessage) {
		if beadID != created.ID {
			return
		}
		if eventType == "bead.closed" {
			close(closedCallbackEntered)
			<-releaseClosedCallback
		}
		notes = append(notes, eventType)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- cache.Close(created.ID) }()
	select {
	case <-closedCallbackEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("close observer was not invoked")
	}

	if err := cache.Reopen(created.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	close(releaseClosedCallback)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Close did not finish after releasing its observer")
	}

	want := []string{"bead.closed", "bead.updated"}
	if !slices.Equal(notes, want) {
		t.Fatalf("notification order = %v, want %v", notes, want)
	}
	for name, store := range map[string]Store{"cache": cache, "backing": base} {
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if got.Status != "open" {
			t.Fatalf("%s status = %q, want open", name, got.Status)
		}
	}
}

func TestCachingStoreCloseObserverCanReenterReopen(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "reentrant reopen"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var (
		cache        *CachingStore
		notes        []string
		reentrantErr error
	)
	cache = NewCachingStoreForTest(base, func(eventType, beadID string, _ json.RawMessage) {
		if beadID != created.ID {
			return
		}
		notes = append(notes, eventType)
		if eventType == "bead.closed" {
			reentrantErr = cache.Reopen(beadID)
		}
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if reentrantErr != nil {
		t.Fatalf("reentrant Reopen: %v", reentrantErr)
	}
	want := []string{"bead.closed", "bead.updated"}
	if !slices.Equal(notes, want) {
		t.Fatalf("notification order = %v, want %v", notes, want)
	}
	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
}

func TestCachingStoreCloseObserverCanReenterGetAndClose(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state cacheState
	}{{"uninitialized", cacheUninitialized}, {"degraded", cacheDegraded}} {
		t.Run(tc.name, func(t *testing.T) {
			backing := NewMemStore()
			created, err := backing.Create(Bead{Title: "reentrant close"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			var (
				cache         *CachingStore
				notifications int
				reentered     bool
				callbackErr   error
			)
			cache = NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
				if eventType != "bead.closed" || beadID != created.ID {
					return
				}
				notifications++
				if reentered {
					return
				}
				reentered = true
				if _, err := cache.Get(created.ID); err != nil {
					callbackErr = err
					return
				}
				callbackErr = cache.Close(created.ID)
			})
			cache.mu.Lock()
			cache.state = tc.state
			cache.mu.Unlock()

			done := make(chan error, 1)
			go func() { done <- cache.Close(created.ID) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("outer Close: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("Close deadlocked while its observer re-entered Get/Close")
			}
			if callbackErr != nil {
				t.Fatalf("reentrant observer: %v", callbackErr)
			}
			if notifications != 1 {
				t.Fatalf("bead.closed notifications = %d, want 1", notifications)
			}
		})
	}
}
