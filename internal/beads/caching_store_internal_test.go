package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCachingStoreRunReconciliationDetectsLabelContentChanges(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "Task", Labels: []string{"old"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := backing.Update(bead.ID, UpdateOpts{
		Labels:       []string{"new"},
		RemoveLabels: []string{"old"},
	}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	cache.runReconciliation()

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "new" {
		t.Fatalf("Labels = %v, want [new]", got.Labels)
	}
}

func TestCachingStoreListInProgressUsesCacheByDefault(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "claimed work",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "in_progress"
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	got, err := cache.List(ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List(in_progress) = %+v, want cached result before reconcile", got)
	}
}

func TestCachingStoreListLiveBypassesCache(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "claimed work",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "in_progress"
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	got, err := cache.List(ListQuery{Status: "in_progress", Live: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("List(in_progress, Live) = %+v, want %s from backing store", got, bead.ID)
	}
}

func TestCachingStoreRunReconciliationDetectsPriorityChanges(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	initialPriority := 1
	bead, err := backing.Create(Bead{Title: "Task", Priority: &initialPriority})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	updatedPriority := 2
	if err := backing.Update(bead.ID, UpdateOpts{Priority: &updatedPriority}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	cache.runReconciliation()

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after reconcile: %v", err)
	}
	if got.Priority == nil || *got.Priority != updatedPriority {
		t.Fatalf("Priority = %v, want %d", got.Priority, updatedPriority)
	}
}

func TestCachingStoreRunReconciliationDetectsDepOnlyChangesAndNotifies(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "Blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	bead, err := backing.Create(Bead{Title: "Task"})
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	deps, err := cache.DepList(bead.ID, "down")
	if err != nil {
		t.Fatalf("DepList before dep add: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("initial deps = %v, want empty", deps)
	}

	if err := backing.DepAdd(bead.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd backing: %v", err)
	}

	cache.runReconciliation()

	deps, err = cache.DepList(bead.ID, "down")
	if err != nil {
		t.Fatalf("DepList after reconcile: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != blocker.ID {
		t.Fatalf("deps after reconcile = %v, want blocker %s", deps, blocker.ID)
	}
	if len(events) != 1 || events[0] != "bead.updated:"+bead.ID {
		t.Fatalf("events = %v, want [bead.updated:%s]", events, bead.ID)
	}
}

func TestCachingStoreRunReconciliationPublishesCallbacksAfterDepsCommitted(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	blocker, err := backing.Create(Bead{Title: "Blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	bead, err := backing.Create(Bead{Title: "Task"})
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}

	var observedDeps int
	var cache *CachingStore
	cache = NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		if eventType != "bead.updated" || beadID != bead.ID {
			return
		}
		deps, err := cache.DepList(beadID, "down")
		if err != nil {
			t.Fatalf("DepList during callback: %v", err)
		}
		observedDeps = len(deps)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if _, err := cache.DepList(bead.ID, "down"); err != nil {
		t.Fatalf("DepList before changes: %v", err)
	}

	title := "Task updated"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}
	if err := backing.DepAdd(bead.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd backing: %v", err)
	}

	cache.runReconciliation()

	if observedDeps != 1 {
		t.Fatalf("observed deps during callback = %d, want 1", observedDeps)
	}
}

func TestCachingStoreUpdateInvalidatesStaleCacheWhenRefreshFails(t *testing.T) {
	t.Parallel()

	backing := &refreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "after"
	backing.failNextGet = true
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Title != "after" {
		t.Fatalf("Title = %q, want after", got.Title)
	}

	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "refresh bead after update") {
		t.Fatalf("LastProblem = %q, want refresh context", stats.LastProblem)
	}
	if stats.LastProblemAt.IsZero() {
		t.Fatal("LastProblemAt should be set")
	}
}

func TestCachingStoreUpdateLogsRefreshFailure(t *testing.T) {
	backing := &refreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	var logged []string
	cache.problemf = func(msg string) {
		logged = append(logged, msg)
	}
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "after"
	backing.failNextGet = true
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(logged) != 1 {
		t.Fatalf("logged = %v, want single refresh failure", logged)
	}
	if !strings.Contains(logged[0], "refresh bead after update") {
		t.Fatalf("logged[0] = %q, want refresh context", logged[0])
	}
	if !strings.Contains(logged[0], bead.ID) {
		t.Fatalf("logged[0] = %q, want bead id", logged[0])
	}
}

func TestCachingStoreDepListUpFallsThroughToBackingTruth(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	root, err := backing.Create(Bead{Title: "root"})
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	left, err := backing.Create(Bead{Title: "left"})
	if err != nil {
		t.Fatalf("Create left: %v", err)
	}
	right, err := backing.Create(Bead{Title: "right"})
	if err != nil {
		t.Fatalf("Create right: %v", err)
	}
	if err := backing.DepAdd(left.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd left: %v", err)
	}
	if err := backing.DepAdd(right.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd right: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Populate only one downward dep entry in the cache, leaving reverse lookups
	// incomplete unless they fall through to the backing store.
	if _, err := cache.DepList(left.ID, "down"); err != nil {
		t.Fatalf("DepList left down: %v", err)
	}

	deps, err := cache.DepList(root.ID, "up")
	if err != nil {
		t.Fatalf("DepList root up: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("DepList(root, up) = %d deps, want 2", len(deps))
	}
}

func TestCachingStoreApplyEventRecordsProblemOnMalformedPayload(t *testing.T) {
	t.Parallel()

	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.ApplyEvent("bead.updated", []byte(`{`))

	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "apply bead.updated event") {
		t.Fatalf("LastProblem = %q, want apply-event context", stats.LastProblem)
	}
	if stats.LastProblemAt.IsZero() {
		t.Fatal("LastProblemAt should be set")
	}
}

func TestCachingStoreRunReconciliationRecordsProblemAndDegrades(t *testing.T) {
	t.Parallel()

	backing := &listFailingStore{Store: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "Task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.failList = true
	for i := 0; i < maxCacheSyncFailures; i++ {
		cache.runReconciliation()
	}

	if cache.state != cacheDegraded {
		t.Fatalf("state = %v, want degraded", cache.state)
	}

	stats := cache.Stats()
	if stats.SyncFailures != maxCacheSyncFailures {
		t.Fatalf("SyncFailures = %d, want %d", stats.SyncFailures, maxCacheSyncFailures)
	}
	if stats.ProblemCount != int64(maxCacheSyncFailures) {
		t.Fatalf("ProblemCount = %d, want %d", stats.ProblemCount, maxCacheSyncFailures)
	}
	if !strings.Contains(stats.LastProblem, "reconcile cache") {
		t.Fatalf("LastProblem = %q, want reconcile context", stats.LastProblem)
	}
}

func TestCachingStoreNextReconcileDelayUsesFreshnessWatchdog(t *testing.T) {
	t.Parallel()

	cache := NewCachingStoreForTest(NewMemStore(), nil)
	cache.state = cacheLive
	cache.lastFreshAt = time.Unix(100, 0)

	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 20*time.Second {
		t.Fatalf("nextReconcileDelay(fresh) = %s, want 20s", got)
	}

	cache.stats.LastReconcileAt = time.Unix(70, 0)
	cache.lastFreshAt = time.Unix(109, 0)
	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 0 {
		t.Fatalf("nextReconcileDelay(stale full scan with fresh writes) = %s, want immediate reconcile", got)
	}

	cache.stats.LastReconcileAt = time.Time{}
	cache.lastFreshAt = time.Unix(70, 0)
	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 0 {
		t.Fatalf("nextReconcileDelay(stale) = %s, want immediate reconcile", got)
	}

	cache.state = cacheDegraded
	cache.lastFreshAt = time.Unix(109, 0)
	if got := cache.nextReconcileDelay(time.Unix(110, 0)); got != 0 {
		t.Fatalf("nextReconcileDelay(degraded) = %s, want immediate reconcile", got)
	}
}

func TestCachingStoreCloseAllRefreshesOnlyActuallyClosedBeads(t *testing.T) {
	t.Parallel()

	backing := &partialCloseAllStore{Store: NewMemStore()}
	first, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := backing.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{first.ID, second.ID}, map[string]string{"source": "wave1"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	gotFirst, err := cache.Get(first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.Status != "closed" {
		t.Fatalf("first status = %q, want closed", gotFirst.Status)
	}
	if gotFirst.Metadata["source"] != "wave1" {
		t.Fatalf("first metadata = %v, want source=wave1", gotFirst.Metadata)
	}

	gotSecond, err := cache.Get(second.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if gotSecond.Status != "open" {
		t.Fatalf("second status = %q, want open", gotSecond.Status)
	}
	if gotSecond.Metadata["source"] != "" {
		t.Fatalf("second metadata = %v, want no source metadata", gotSecond.Metadata)
	}
}

func TestCachingStoreCloseAllRefreshesPartialSuccessBeforeReturningError(t *testing.T) {
	t.Parallel()

	backing := &partialCloseAllErrorStore{Store: NewMemStore()}
	first, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := backing.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{first.ID, second.ID}, map[string]string{"source": "wave1"})
	if err == nil {
		t.Fatal("expected CloseAll error")
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	gotFirst, err := cache.Get(first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.Status != "closed" {
		t.Fatalf("first status = %q, want closed", gotFirst.Status)
	}
	if gotFirst.Metadata["source"] != "wave1" {
		t.Fatalf("first metadata = %v, want source=wave1", gotFirst.Metadata)
	}
	stats := cache.Stats()
	if stats.State != "live" {
		t.Fatalf("cache state = %q, want live", stats.State)
	}
}

func TestCachingStoreCloseAllRefreshesNonPrefixPartialSuccess(t *testing.T) {
	t.Parallel()

	backing := &nonPrefixCloseAllErrorStore{Store: NewMemStore()}
	first, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := backing.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{first.ID, second.ID}, map[string]string{"source": "wave1"})
	if err == nil {
		t.Fatal("expected CloseAll error")
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	gotFirst, err := cache.Get(first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.Status != "open" {
		t.Fatalf("first status = %q, want open", gotFirst.Status)
	}
	gotSecond, err := cache.Get(second.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if gotSecond.Status != "closed" {
		t.Fatalf("second status = %q, want closed", gotSecond.Status)
	}
	if gotSecond.Metadata["source"] != "wave1" {
		t.Fatalf("second metadata = %v, want source=wave1", gotSecond.Metadata)
	}
}

func TestCachingStoreCloseAllMarksRefreshFailuresDirty(t *testing.T) {
	t.Parallel()

	backing := &closeAllRefreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.failGetID = bead.ID
	closed, err := cache.CloseAll([]string{bead.ID}, nil)
	if err == nil {
		t.Fatal("expected CloseAll refresh error")
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1", closed)
	}

	if _, err := cache.List(ListQuery{AllowScan: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if backing.listCalls == 0 {
		t.Fatal("List did not fall back to backing store after dirty refresh failure")
	}
}

func TestCachingStoreCachedListReturnsSnapshotWithDirtyEntries(t *testing.T) {
	t.Parallel()

	backing := &refreshFailingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "active work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	title := "updated while refresh fails"
	backing.failNextGet = true
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	rows, ok := cache.CachedList(ListQuery{Status: "open"})
	if !ok {
		t.Fatal("CachedList returned ok=false for dirty cache, want snapshot")
	}
	if len(rows) != 1 || rows[0].ID != bead.ID {
		t.Fatalf("CachedList = %#v, want dirty snapshot row %s", rows, bead.ID)
	}
	if rows[0].Title == title {
		t.Fatalf("CachedList returned refreshed title %q; test setup did not create a dirty stale snapshot", rows[0].Title)
	}
}

type refreshFailingStore struct {
	Store
	failNextGet bool
}

func (s *refreshFailingStore) Get(id string) (Bead, error) {
	if s.failNextGet {
		s.failNextGet = false
		return Bead{}, errors.New("transient get failure")
	}
	return s.Store.Get(id)
}

type listFailingStore struct {
	Store
	failList bool
}

func (s *listFailingStore) List(query ListQuery) ([]Bead, error) {
	if s.failList {
		return nil, errors.New("transient list failure")
	}
	return s.Store.List(query)
}

type partialCloseAllStore struct {
	Store
}

func (s *partialCloseAllStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.SetMetadataBatch(ids[0], metadata); err != nil {
		return 0, err
	}
	if err := s.Close(ids[0]); err != nil {
		return 0, err
	}
	return 1, nil
}

type partialCloseAllErrorStore struct {
	Store
}

func (s *partialCloseAllErrorStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, errors.New("no ids")
	}
	if err := s.SetMetadataBatch(ids[0], metadata); err != nil {
		return 0, err
	}
	if err := s.Close(ids[0]); err != nil {
		return 0, err
	}
	return 1, errors.New("second close failed")
}

type nonPrefixCloseAllErrorStore struct {
	Store
}

func (s *nonPrefixCloseAllErrorStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if len(ids) < 2 {
		return 0, errors.New("need two ids")
	}
	if err := s.SetMetadataBatch(ids[1], metadata); err != nil {
		return 0, err
	}
	if err := s.Close(ids[1]); err != nil {
		return 0, err
	}
	return 1, errors.New("first close failed")
}

type closeAllRefreshFailingStore struct {
	Store
	failGetID string
	listCalls int
}

func (s *closeAllRefreshFailingStore) Get(id string) (Bead, error) {
	if id == s.failGetID {
		s.failGetID = ""
		return Bead{}, errors.New("refresh failed")
	}
	return s.Store.Get(id)
}

func (s *closeAllRefreshFailingStore) CloseAll(ids []string, _ map[string]string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if err := s.Close(ids[0]); err != nil {
		return 0, err
	}
	return 1, nil
}

func (s *closeAllRefreshFailingStore) List(query ListQuery) ([]Bead, error) {
	s.listCalls++
	return s.Store.List(query)
}

// Reconciliation must not re-emit bead.closed for a cache entry whose status
// is already "closed". When ApplyEvent ingests an external bead.closed event
// (from the bus), it stores the closed bead in c.beads. List({AllowScan:true})
// filters out closed beads, so the next reconcile sees the entry as missing
// from the fresh DB read and would re-emit a duplicate close notification.
// Routed back through the event bus, that notification re-applies into every
// caching store and reconciles into another spurious close — the storm.
func TestCachingStoreRunReconciliationDoesNotEmitBeadClosedForAlreadyClosedCacheEntry(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// External writer closes the bead in the backing store, then the close
	// event is delivered through the bus and applied to this cache.
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("backing Close: %v", err)
	}
	closed := bead
	closed.Status = "closed"
	payload, err := json.Marshal(closed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cache.ApplyEvent("bead.closed", payload)
	events = nil // ignore notifications from prime/apply; only assert on reconcile output

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+bead.ID {
			t.Fatalf("reconciler emitted duplicate bead.closed for an already-closed cache entry; events=%v", events)
		}
	}
}
