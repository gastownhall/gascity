package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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

func TestCachingStoreCreateWithStorageForwardsPolicyStorageAndCachesResult(t *testing.T) {
	backing := &storageCreateRecordingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)

	created, err := cache.CreateWithStorage(Bead{Title: "session"}, StorageNoHistory)
	if err != nil {
		t.Fatalf("CreateWithStorage: %v", err)
	}

	if backing.storage != StorageNoHistory {
		t.Fatalf("backing storage = %q, want %q", backing.storage, StorageNoHistory)
	}
	if !created.NoHistory || created.Ephemeral {
		t.Fatalf("created storage = ephemeral:%v no_history:%v, want no-history", created.Ephemeral, created.NoHistory)
	}
	cached, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("cache Get: %v", err)
	}
	if cached.ID != created.ID || !cached.NoHistory || cached.Ephemeral {
		t.Fatalf("cached bead = %+v, want no-history created bead %s", cached, created.ID)
	}
}

func TestCachingStoreGraphApplyHandleForwardsStorageAndCachesResult(t *testing.T) {
	backing := &storageGraphApplyRecordingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	applier, ok := GraphApplyFor(cache)
	if !ok {
		t.Fatal("GraphApplyFor(cache) = false, want graph handle from backing store")
	}
	storageApplier, ok := applier.(StorageGraphApplyStore)
	if !ok {
		t.Fatal("GraphApplyFor(cache) did not preserve StorageGraphApplyStore")
	}

	result, err := storageApplier.ApplyGraphPlanWithStorage(t.Context(), &GraphApplyPlan{
		Nodes: []GraphApplyNode{{Key: "root", Title: "Root"}},
	}, StorageEphemeral)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}
	if backing.storage != StorageEphemeral {
		t.Fatalf("backing storage = %q, want %q", backing.storage, StorageEphemeral)
	}
	cached, err := cache.Get(result.IDs["root"])
	if err != nil {
		t.Fatalf("cache Get(graph root): %v", err)
	}
	if !cached.Ephemeral || cached.NoHistory {
		t.Fatalf("cached graph root storage = ephemeral:%v no_history:%v, want ephemeral", cached.Ephemeral, cached.NoHistory)
	}
}

func TestGraphApplyForCachingStoreWithoutGraphBackingReturnsFalse(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if _, ok := GraphApplyFor(cache); ok {
		t.Fatal("GraphApplyFor(cache with plain backing) = true, want false")
	}
}

type storageCreateRecordingStore struct {
	Store
	storage StorageClass
}

func (s *storageCreateRecordingStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	s.storage = storage
	switch storage {
	case StorageNoHistory:
		b.NoHistory = true
		b.Ephemeral = false
	case StorageEphemeral:
		b.Ephemeral = true
		b.NoHistory = false
	case StorageHistory:
		b.Ephemeral = false
		b.NoHistory = false
	}
	return s.Create(b)
}

type storageGraphApplyRecordingStore struct {
	Store
	storage StorageClass
}

func (s *storageGraphApplyRecordingStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

func (s *storageGraphApplyRecordingStore) ApplyGraphPlanWithStorage(_ context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	s.storage = storage
	ids := make(map[string]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		metadata := make(map[string]string, len(node.Metadata))
		for key, value := range node.Metadata {
			metadata[key] = value
		}
		created, err := s.Create(Bead{
			Title:     node.Title,
			Type:      node.Type,
			Metadata:  metadata,
			Ephemeral: storage == StorageEphemeral,
			NoHistory: storage == StorageNoHistory,
		})
		if err != nil {
			return nil, err
		}
		ids[node.Key] = created.ID
	}
	return &GraphApplyResult{IDs: ids}, nil
}

func TestCachingStoreRunReconciliationSkipLabelsSuppressesLabelOnlyUpdates(t *testing.T) {
	t.Parallel()

	backing := &skipLabelsRecordingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "Task", Labels: []string{"foo"}})
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
	if got := backing.lastListQuery(t); !got.SkipLabels {
		t.Fatalf("Prime List query SkipLabels = false, want true")
	}

	backing.dropLabels = true
	cache.runReconciliation()
	if got := backing.lastListQuery(t); !got.SkipLabels {
		t.Fatalf("reconcile List query SkipLabels = false, want true")
	}
	if len(events) != 0 {
		t.Fatalf("events after label-only reconcile = %v, want none", events)
	}

	status := "in_progress"
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update backing status: %v", err)
	}
	cache.runReconciliation()
	if len(events) != 1 || events[0] != "bead.updated:"+bead.ID {
		t.Fatalf("events after status reconcile = %v, want [bead.updated:%s]", events, bead.ID)
	}
}

type skipLabelsRecordingStore struct {
	Store
	dropLabels  bool
	listQueries []ListQuery
}

func (s *skipLabelsRecordingStore) List(query ListQuery) ([]Bead, error) {
	s.listQueries = append(s.listQueries, query)
	rows, err := s.Store.List(query)
	if err != nil || !query.SkipLabels || !s.dropLabels {
		return rows, err
	}
	out := make([]Bead, len(rows))
	for i, row := range rows {
		out[i] = cloneBead(row)
		out[i].Labels = nil
	}
	return out, nil
}

func (s *skipLabelsRecordingStore) lastListQuery(t *testing.T) ListQuery {
	t.Helper()
	if len(s.listQueries) == 0 {
		t.Fatal("no List query recorded")
	}
	return s.listQueries[len(s.listQueries)-1]
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

func TestCachingStoreListLiveInvalidatesCachedRowsMissingFromBacking(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "work",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	live, err := cache.List(ListQuery{Status: "open", Assignee: "worker", Live: true})
	if err != nil {
		t.Fatalf("List live: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("Live List(open) = %+v, want closed bead omitted", live)
	}

	cached, err := cache.Handles().Cached.List(ListQuery{Status: "open", Assignee: "worker"})
	if err != nil {
		t.Fatalf("Cached List(open): %v", err)
	}
	if len(cached) != 0 {
		t.Fatalf("Cached List(open) after live refresh = %+v, want stale bead invalidated", cached)
	}

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("Get status after live refresh = %q, want closed", got.Status)
	}
}

func TestCachingStoreHandlesCachedReadsShareFullPrime(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "cached work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &blockingPrimeListStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	cache := NewCachingStoreForTest(backing, nil)
	handles := cache.Handles()

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		rows, err := handles.Cached.List(ListQuery{Status: "open"})
		if err == nil && (len(rows) != 1 || rows[0].ID != bead.ID) {
			err = fmt.Errorf("cached List rows = %#v, want %s", rows, bead.ID)
		}
		errs <- err
	}()
	go func() {
		defer wg.Done()
		rows, err := handles.Cached.Ready()
		if err == nil && (len(rows) != 1 || rows[0].ID != bead.ID) {
			err = fmt.Errorf("cached Ready rows = %#v, want %s", rows, bead.ID)
		}
		errs <- err
	}()
	go func() {
		defer wg.Done()
		if _, err := handles.Cached.DepList(bead.ID, "down"); err != nil {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case <-backing.started:
	case <-time.After(time.Second):
		t.Fatal("cached reads did not start shared full prime")
	}
	time.Sleep(25 * time.Millisecond)
	if got := backing.primeListCalls.Load(); got != 1 {
		t.Fatalf("prime list calls while cached reads blocked = %d, want 1", got)
	}

	close(backing.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := backing.primeListCalls.Load(); got != 1 {
		t.Fatalf("total prime list calls = %d, want 1", got)
	}
}

func TestCachingStoreHandlesCachedListUsesActiveSnapshotAfterPrimeActive(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "cached work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &blockingPrimeListStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		rows, err := cache.Handles().Cached.List(ListQuery{Status: "open"})
		if err == nil && (len(rows) != 1 || rows[0].ID != bead.ID) {
			err = fmt.Errorf("cached List rows = %#v, want %s", rows, bead.ID)
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-backing.started:
		t.Fatal("cached active List started lazy full prime after PrimeActive")
	case <-time.After(25 * time.Millisecond):
		t.Fatal("cached active List did not return promptly from PrimeActive snapshot")
	}

	close(backing.release)
	if got := backing.primeListCalls.Load(); got != 0 {
		t.Fatalf("full-prime list calls = %d, want none for active cached List", got)
	}
}

func TestCachingStoreHandlesCachedReadUsesActiveSnapshotDuringRunningFullPrimeAfterPrimeActive(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "cached work"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &blockingPrimeListStore{
		Store:   mem,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	primeDone := make(chan error, 1)
	go func() {
		primeDone <- cache.Prime(context.Background())
	}()
	select {
	case <-backing.started:
	case <-time.After(time.Second):
		t.Fatal("full prime did not start")
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := cache.Handles().Cached.List(ListQuery{Status: "open"})
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("Cached.List error = %v, want active snapshot result", err)
		}
	case <-time.After(25 * time.Millisecond):
		t.Fatal("Cached.List waited for the running full prime")
	}
	if got := backing.primeListCalls.Load(); got != 1 {
		t.Fatalf("prime list calls = %d, want only the running full prime", got)
	}

	close(backing.release)
	if err := <-primeDone; err != nil {
		t.Fatalf("Prime: %v", err)
	}
}

func TestCachingStoreHandlesCachedReadDoesNotPrimeWhenDegraded(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	if _, err := mem.Create(Bead{
		Title:  "cached work",
		Status: "open",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &hardFailFullPrimeStore{
		Store: mem,
		err:   errors.New("full scan unavailable"),
	}
	cache := NewCachingStoreForTest(backing, nil)
	cache.mu.Lock()
	cache.state = cacheDegraded
	cache.mu.Unlock()

	_, err := cache.Handles().Cached.List(ListQuery{Status: "open"})
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Cached.List error = %v, want ErrCacheUnavailable", err)
	}
	if got := backing.primeListCalls.Load(); got != 0 {
		t.Fatalf("full-prime list calls = %d, want no synchronous prime while degraded", got)
	}
}

func TestCachingStoreHandlesCachedReadsSuppressRecentPartialPrimeRetry(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "cached work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &countingPartialFullPrimeStore{
		partialListErrorStore: &partialListErrorStore{
			Store:            mem,
			partialAllowScan: true,
			partialRows:      []Bead{bead},
		},
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if got := backing.primeListCalls.Load(); got != 1 {
		t.Fatalf("prime list calls after partial Prime = %d, want 1", got)
	}

	for i := 0; i < 2; i++ {
		if _, err := cache.Handles().Cached.List(ListQuery{Status: "open"}); !errors.Is(err, ErrCacheUnavailable) {
			t.Fatalf("Cached.List attempt %d error = %v, want ErrCacheUnavailable", i+1, err)
		}
	}
	if got := backing.primeListCalls.Load(); got != 1 {
		t.Fatalf("prime list calls after suppressed cached reads = %d, want 1", got)
	}

	backing.partialAllowScan = false
	cache.primeMu.Lock()
	cache.lastFullPrimeStartedAt = time.Now().Add(-cacheLazyFullPrimeRetryInterval - time.Second)
	cache.primeMu.Unlock()

	rows, err := cache.Handles().Cached.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("Cached.List after retry interval: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != bead.ID {
		t.Fatalf("Cached.List rows after retry = %#v, want %s", rows, bead.ID)
	}
	if got := backing.primeListCalls.Load(); got != 2 {
		t.Fatalf("prime list calls after retry interval = %d, want 2", got)
	}
}

func TestCachingStoreHandlesCachedListHardPrimeFailureReturnsCacheUnavailable(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	work, err := mem.Create(Bead{
		Title:    "active work",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := mem.Update(work.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}
	backing := &hardFailFullPrimeStore{
		Store: mem,
		err:   errors.New("full scan unavailable"),
	}
	cache := NewCachingStoreForTest(backing, nil)
	cache.primeRetryDelay = func(int) time.Duration { return 0 }

	_, err = cache.Handles().Cached.List(ListQuery{Status: "in_progress"})
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Cached.List error = %v, want ErrCacheUnavailable", err)
	}
	if !strings.Contains(err.Error(), "full scan unavailable") {
		t.Fatalf("Cached.List error = %v, want hard prime cause preserved", err)
	}
	if got := backing.primeListCalls.Load(); got != 3 {
		t.Fatalf("full-prime list calls = %d, want 3 retries", got)
	}

	rows, err := cache.Handles().Live.List(ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("Live.List: %v", err)
	}
	assertHasBeadIDs(t, rows, work.ID)
	if got := backing.liveInProgressLists.Load(); got != 1 {
		t.Fatalf("live in-progress list calls = %d, want targeted live fallback path to succeed", got)
	}
}

func TestCachingStorePrimeWaiterReturnsGenerationError(t *testing.T) {
	t.Parallel()

	cache := NewCachingStoreForTest(NewMemStore(), nil)
	cycle, owner := cache.beginFullPrime()
	if !owner {
		t.Fatal("first beginFullPrime did not return owner")
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cache.waitForFullPrimeDone(context.Background(), cycle)
	}()

	firstErr := errors.New("first generation failed")
	cache.primeMu.Lock()
	cycle.err = firstErr
	cache.primeRunning = false
	close(cycle.done)
	cache.primeCycle = &fullPrimeCycle{done: make(chan struct{})}
	cache.primeRunning = true
	cache.lastFullPrimeStartedAt = time.Now()
	cache.primeMu.Unlock()

	if err := <-waitErr; !errors.Is(err, firstErr) {
		t.Fatalf("waitForFullPrimeDone error = %v, want first generation error", err)
	}
}

func TestCachingStoreHandlesReadLogicalStoreWithoutTierFlags(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	issue, err := backing.Create(Bead{Title: "issue work"})
	if err != nil {
		t.Fatalf("Create issue: %v", err)
	}
	wisp, err := backing.Create(Bead{Title: "wisp work", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)

	cachedRows, err := cache.Handles().Cached.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("Cached.List: %v", err)
	}
	assertHasBeadIDs(t, cachedRows, issue.ID, wisp.ID)

	inProgress := "in_progress"
	if err := backing.Update(wisp.ID, UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("Update backing wisp: %v", err)
	}
	liveRows, err := cache.Handles().Live.List(ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("Live.List: %v", err)
	}
	assertHasBeadIDs(t, liveRows, wisp.ID)
}

func TestHandlesForPlainStoreReadsLogicalBothTiers(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	issue, err := store.Create(Bead{Title: "issue work"})
	if err != nil {
		t.Fatalf("Create issue: %v", err)
	}
	wisp, err := store.Create(Bead{Title: "wisp work", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}

	cachedRows, err := HandlesFor(store).Cached.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("Cached.List: %v", err)
	}
	assertHasBeadIDs(t, cachedRows, issue.ID, wisp.ID)

	liveRows, err := HandlesFor(store).Live.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("Live.List: %v", err)
	}
	assertHasBeadIDs(t, liveRows, issue.ID, wisp.ID)
}

func TestCachingStoreListWispsUsesCacheByDefault(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	wisp, err := backing.Create(Bead{
		Title:     "wisp work",
		Assignee:  "worker",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	status := "in_progress"
	if err := backing.Update(wisp.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	got, err := cache.List(ListQuery{Status: "open", Assignee: "worker", TierMode: TierWisps})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != wisp.ID {
		t.Fatalf("List(open wisp) = %+v, want cached %s", got, wisp.ID)
	}
}

type blockingPrimeListStore struct {
	Store
	started        chan struct{}
	release        chan struct{}
	startedOnce    sync.Once
	primeListCalls atomic.Int64
}

func (s *blockingPrimeListStore) List(query ListQuery) ([]Bead, error) {
	if query.AllowScan && query.SkipLabels {
		s.primeListCalls.Add(1)
		s.startedOnce.Do(func() { close(s.started) })
		<-s.release
	}
	return s.Store.List(query)
}

type countingPartialFullPrimeStore struct {
	*partialListErrorStore
	primeListCalls atomic.Int64
}

func (s *countingPartialFullPrimeStore) List(query ListQuery) ([]Bead, error) {
	if query.AllowScan && query.SkipLabels && query.TierMode == TierBoth {
		s.primeListCalls.Add(1)
	}
	return s.partialListErrorStore.List(query)
}

type hardFailFullPrimeStore struct {
	Store
	err                 error
	primeListCalls      atomic.Int64
	liveInProgressLists atomic.Int64
}

func (s *hardFailFullPrimeStore) List(query ListQuery) ([]Bead, error) {
	if !query.Live && query.AllowScan && query.SkipLabels && query.TierMode == TierBoth {
		s.primeListCalls.Add(1)
		return nil, s.err
	}
	if query.Live && query.Status == "in_progress" {
		s.liveInProgressLists.Add(1)
	}
	return s.Store.List(query)
}

func assertHasBeadIDs(t *testing.T, rows []Bead, want ...string) {
	t.Helper()
	got := make(map[string]bool, len(rows))
	for _, row := range rows {
		got[row.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("rows ids = %v rows=%#v, missing %s", got, rows, id)
		}
	}
	if len(rows) != len(want) {
		t.Fatalf("rows ids = %v rows=%#v, want exactly %v", got, rows, want)
	}
}

func TestCachingStoreListBothTiersUsesCachedWispsByDefault(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	issue, err := backing.Create(Bead{
		Title:    "issue work",
		Assignee: "worker",
	})
	if err != nil {
		t.Fatalf("Create issue: %v", err)
	}
	wisp, err := backing.Create(Bead{
		Title:     "wisp work",
		Assignee:  "worker",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	status := "in_progress"
	if err := backing.Update(wisp.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update backing: %v", err)
	}

	got, err := cache.List(ListQuery{Status: "open", Assignee: "worker", TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, bead := range got {
		ids[bead.ID] = true
	}
	if !ids[issue.ID] || !ids[wisp.ID] || len(got) != 2 {
		t.Fatalf("List(open both tiers) ids = %v rows=%+v, want cached issue %s and cached wisp %s", ids, got, issue.ID, wisp.ID)
	}
}

func TestCachingStoreApplyEventRecordsBackingVerificationErrorAndAppliesUpdate(t *testing.T) {
	t.Parallel()

	backing := &cacheEventVerificationFailStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "original"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	localTitle := "local"
	if err := cache.Update(bead.ID, UpdateOpts{Title: &localTitle}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	cache.mu.Lock()
	delete(cache.beadSeq, bead.ID)
	cache.mu.Unlock()
	backing.failNextGet = true

	cache.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+bead.ID+`","title":"external"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "external" {
		t.Fatalf("Title after verification error = %q, want external", got.Title)
	}
	stats := cache.Stats()
	if stats.ProblemCount == 0 {
		t.Fatal("ProblemCount = 0, want verification error recorded")
	}
	if !strings.Contains(stats.LastProblem, "verify bead.updated event") {
		t.Fatalf("LastProblem = %q, want verify bead.updated event", stats.LastProblem)
	}
}

func TestCachingStoreIgnoresStaleClosedEventAfterLocalReopenBeyondRecentWindow(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	cache.mu.Lock()
	cache.localBeadAt[bead.ID] = time.Now().Add(-10 * time.Second)
	cache.mu.Unlock()

	cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+bead.ID+`","status":"closed"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after stale closed event = %q, want open", got.Status)
	}
}

func TestCachingStoreIgnoresStaleClosedEventAfterLocalReopenAndLiveRefresh(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	cache.mu.Lock()
	cache.localBeadAt[bead.ID] = time.Now().Add(-10 * time.Second)
	cache.mu.Unlock()
	if got, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatalf("Live List: %v", err)
	} else if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("Live List = %+v, want reopened bead %s", got, bead.ID)
	}
	cache.mu.RLock()
	_, locallyMutated := cache.beadSeq[bead.ID]
	recentlyLocal := recentLocalMutation(cache.localBeadAt[bead.ID], time.Now())
	cache.mu.RUnlock()
	if locallyMutated || recentlyLocal {
		t.Fatalf("local markers after live refresh: locallyMutated=%v recentlyLocal=%v, want both false", locallyMutated, recentlyLocal)
	}

	cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+bead.ID+`","status":"closed"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after stale closed event = %q, want open", got.Status)
	}
}

func TestCachingStoreClosedEventRechecksLocalReopenBeforeCommit(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}
	payload := json.RawMessage(`{"id":"` + bead.ID + `","status":"closed"}`)
	cache.ApplyEvent("bead.closed", payload)

	beforeCommit := make(chan struct{})
	releaseCommit := make(chan struct{})
	cache.applyEventBeforeCommitForTest = func() {
		close(beforeCommit)
		<-releaseCommit
	}

	done := make(chan struct{})
	go func() {
		cache.ApplyEvent("bead.closed", payload)
		close(done)
	}()

	<-beforeCommit
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	close(releaseCommit)
	<-done

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after stale closed event race = %q, want open", got.Status)
	}
}

func TestCachingStoreRecordsClosedEventVerificationErrorAndPreservesLocalReopen(t *testing.T) {
	backing := &cacheEventVerificationFailStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "reopen me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Reopen(bead.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	backing.failNextGet = true

	cache.ApplyEvent("bead.closed", json.RawMessage(`{"id":"`+bead.ID+`","status":"closed"}`))

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status after verification error = %q, want open", got.Status)
	}
	stats := cache.Stats()
	if stats.ProblemCount == 0 {
		t.Fatal("ProblemCount = 0, want verification error recorded")
	}
	if !strings.Contains(stats.LastProblem, "verify bead.closed event") {
		t.Fatalf("LastProblem = %q, want verify bead.closed event", stats.LastProblem)
	}
}

func TestCachingStoreClosedEventRefreshesStalePayloadFromBacking(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "close me", Metadata: map[string]string{"gc.step_ref": "old"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "closed"
	if err := backing.Update(bead.ID, UpdateOpts{
		Status: &status,
		Metadata: map[string]string{
			"ci.verdict": "done",
			"gc.outcome": "pass",
		},
	}); err != nil {
		t.Fatalf("Update backing close metadata: %v", err)
	}

	stalePayload, err := json.Marshal(Bead{
		ID:        bead.ID,
		Status:    "closed",
		UpdatedAt: bead.UpdatedAt,
		Metadata: map[string]string{
			"gc.step_ref": "old",
		},
	})
	if err != nil {
		t.Fatalf("Marshal stale payload: %v", err)
	}
	cache.ApplyEvent("bead.closed", stalePayload)

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("Status after close event = %q, want closed", got.Status)
	}
	if got.Metadata["ci.verdict"] != "done" || got.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("metadata after stale close event = %#v, want fresh backing metadata", got.Metadata)
	}
}

func TestCachingStoreClosedEventRefreshesBackingForMissingZeroOrEqualUpdatedAt(t *testing.T) {
	type testCase struct {
		name    string
		payload func(t *testing.T, id string, fresh Bead) json.RawMessage
		wantRef bool
	}

	cases := []testCase{
		{
			name: "missing updated_at",
			payload: func(t *testing.T, id string, _ Bead) json.RawMessage {
				t.Helper()
				return json.RawMessage(fmt.Sprintf(`{"id":%q,"status":"closed"}`, id))
			},
			wantRef: true,
		},
		{
			name: "zero updated_at",
			payload: func(t *testing.T, id string, _ Bead) json.RawMessage {
				t.Helper()
				return json.RawMessage(fmt.Sprintf(
					`{"id":%q,"status":"closed","updated_at":"0001-01-01T00:00:00Z"}`,
					id,
				))
			},
			wantRef: true,
		},
		{
			name: "equal updated_at",
			payload: func(t *testing.T, id string, fresh Bead) json.RawMessage {
				t.Helper()
				return json.RawMessage(fmt.Sprintf(
					`{"id":%q,"status":"closed","updated_at":%q}`,
					id,
					fresh.UpdatedAt.Format(time.RFC3339Nano),
				))
			},
			wantRef: true,
		},
		{
			name: "newer updated_at",
			payload: func(t *testing.T, id string, fresh Bead) json.RawMessage {
				t.Helper()
				return json.RawMessage(fmt.Sprintf(
					`{"id":%q,"status":"closed","updated_at":%q,"metadata":{"gc.step_ref":"new"}}`,
					id,
					fresh.UpdatedAt.Add(time.Nanosecond).Format(time.RFC3339Nano),
				))
			},
			wantRef: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backing := NewMemStore()
			bead, err := backing.Create(Bead{Title: "close me", Metadata: map[string]string{"gc.step_ref": "old"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			cache := NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			status := "closed"
			if err := backing.Update(bead.ID, UpdateOpts{
				Status: &status,
				Metadata: map[string]string{
					"ci.verdict": "done",
					"gc.outcome": "pass",
				},
			}); err != nil {
				t.Fatalf("Update backing close metadata: %v", err)
			}
			fresh, err := backing.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get backing: %v", err)
			}
			cache.ApplyEvent("bead.closed", tc.payload(t, bead.ID, fresh))

			got, err := cache.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != "closed" {
				t.Fatalf("Status after close event = %q, want closed", got.Status)
			}
			if tc.wantRef {
				if got.Metadata["ci.verdict"] != "done" || got.Metadata["gc.outcome"] != "pass" {
					t.Fatalf("metadata after close event = %#v, want fresh backing metadata", got.Metadata)
				}
				return
			}
			if got.Metadata["gc.step_ref"] != "new" {
				t.Fatalf("metadata after newer close event = %#v, want newer payload metadata", got.Metadata)
			}
			if _, ok := got.Metadata["ci.verdict"]; ok {
				t.Fatalf("metadata after newer close event = %#v, want merge path to skip backing refresh", got.Metadata)
			}
		})
	}
}

type cacheEventVerificationFailStore struct {
	Store
	failNextGet bool
}

func (s *cacheEventVerificationFailStore) Get(id string) (Bead, error) {
	if s.failNextGet {
		s.failNextGet = false
		return Bead{}, errors.New("backing verification failed")
	}
	return s.Store.Get(id)
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

func TestCachingStoreUpdateRemovesCacheWhenRefreshReturnsNotFound(t *testing.T) {
	t.Parallel()

	backing := &deleteAfterUpdateStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "before"})
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

	title := "after"
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got, err := cache.Get(bead.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after update/refresh NotFound = (%#v, %v), want ErrNotFound", got, err)
	}
	items, err := cache.List(ListQuery{Status: "open", AllowScan: true})
	if err != nil {
		t.Fatalf("List after update/refresh NotFound: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("List after update/refresh NotFound = %#v, want no resurrected bead", items)
	}
	if len(events) != 1 || events[0] != "bead.closed:"+bead.ID {
		t.Fatalf("events = %v, want [bead.closed:%s]", events, bead.ID)
	}
	stats := cache.Stats()
	if stats.ProblemCount != 0 {
		t.Fatalf("ProblemCount = %d, want benign refresh NotFound to stay out of problem log", stats.ProblemCount)
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

func TestCachingStoreSparseUpdatedEventFallsBackWhenCompleteCoverageIsMissingDeps(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	cache.mu.Lock()
	delete(cache.deps, bead.ID)
	cache.depsComplete = true
	cache.mu.Unlock()

	cache.ApplyEvent("bead.updated", json.RawMessage(`{"id":"`+bead.ID+`","title":"target"}`))

	cache.mu.RLock()
	depsComplete := cache.depsComplete
	lastProblem := cache.stats.LastProblem
	cache.mu.RUnlock()
	if depsComplete {
		t.Fatal("depsComplete = true, want incomplete coverage after missing deps invariant break")
	}
	if !strings.Contains(lastProblem, "missing deps for "+bead.ID) {
		t.Fatalf("LastProblem = %q, want missing deps diagnostic for %s", lastProblem, bead.ID)
	}
	if _, ok := cache.CachedReady(); ok {
		t.Fatal("CachedReady answered from cache after dependency coverage became incomplete")
	}
}

func TestCachingStoreNoOpUpdatedEventSequencesDependencyCoverageInvalidation(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	cache.mu.Lock()
	cache.deps[bead.ID] = nil
	cache.depsComplete = true
	cache.mu.Unlock()

	cache.mu.RLock()
	startMutationSeq := cache.mutationSeq
	cache.mu.RUnlock()

	payload, err := json.Marshal(bead)
	if err != nil {
		t.Fatalf("Marshal bead: %v", err)
	}
	cache.ApplyEvent("bead.updated", payload)

	cache.mu.RLock()
	gotMutationSeq := cache.mutationSeq
	gotBeadSeq, hasBeadSeq := cache.beadSeq[bead.ID]
	depsComplete := cache.depsComplete
	_, hasDeps := cache.deps[bead.ID]
	cache.mu.RUnlock()
	if gotMutationSeq <= startMutationSeq {
		t.Fatalf("mutationSeq = %d, want advanced past %d", gotMutationSeq, startMutationSeq)
	}
	if !hasBeadSeq || gotBeadSeq <= startMutationSeq {
		t.Fatalf("beadSeq[%s] = (%d, %v), want sequenced after %d", bead.ID, gotBeadSeq, hasBeadSeq, startMutationSeq)
	}
	if depsComplete {
		t.Fatal("depsComplete = true, want dependency-omitting update to mark coverage incomplete")
	}
	if hasDeps {
		t.Fatalf("deps[%s] still cached after dependency-omitting update", bead.ID)
	}
	if ready, ok := cache.CachedReady(); ok {
		t.Fatalf("CachedReady answered from cache after dependency coverage became incomplete: %v", ready)
	}
}

func TestCachingStoreNoOpUpdatedEventPreservesCachedMetadataMap(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "target",
		Metadata: map[string]string{"key": "value"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	cache.mu.RLock()
	startMutationSeq := cache.mutationSeq
	beforeMetadata := reflect.ValueOf(cache.beads[bead.ID].Metadata).Pointer()
	cache.mu.RUnlock()

	payload, err := json.Marshal(struct {
		ID           string            `json:"id"`
		Title        string            `json:"title"`
		Status       string            `json:"status"`
		Type         string            `json:"issue_type"`
		CreatedAt    time.Time         `json:"created_at"`
		Metadata     map[string]string `json:"metadata"`
		Dependencies []Dep             `json:"dependencies"`
	}{
		ID:           bead.ID,
		Title:        bead.Title,
		Status:       bead.Status,
		Type:         bead.Type,
		CreatedAt:    bead.CreatedAt,
		Metadata:     bead.Metadata,
		Dependencies: []Dep{},
	})
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	cache.ApplyEvent("bead.updated", payload)

	cache.mu.RLock()
	gotMutationSeq := cache.mutationSeq
	afterMetadata := reflect.ValueOf(cache.beads[bead.ID].Metadata).Pointer()
	cache.mu.RUnlock()
	if gotMutationSeq != startMutationSeq {
		t.Fatalf("mutationSeq = %d, want unchanged %d", gotMutationSeq, startMutationSeq)
	}
	if afterMetadata != beforeMetadata {
		t.Fatalf("metadata map pointer = %x, want unchanged %x", afterMetadata, beforeMetadata)
	}
}

func TestCachingStoreApplyEventRechecksLocalMutationBeforeCommit(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "mail",
		Type:     "message",
		Labels:   []string{"thread:abc"},
		Metadata: map[string]string{"mail.read": "false"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.Update(bead.ID, UpdateOpts{
		Labels:   []string{"read"},
		Metadata: map[string]string{"mail.read": "true"},
	}); err != nil {
		t.Fatalf("Mark read update: %v", err)
	}
	staleRead, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get stale read payload: %v", err)
	}
	payload, err := json.Marshal(staleRead)
	if err != nil {
		t.Fatalf("Marshal stale read payload: %v", err)
	}

	beforeCommit := make(chan struct{})
	releaseCommit := make(chan struct{})
	cache.applyEventBeforeCommitForTest = func() {
		close(beforeCommit)
		<-releaseCommit
	}

	done := make(chan struct{})
	go func() {
		cache.ApplyEvent("bead.updated", payload)
		close(done)
	}()

	<-beforeCommit
	if err := cache.Update(bead.ID, UpdateOpts{
		RemoveLabels: []string{"read"},
		Metadata:     map[string]string{"mail.read": "false"},
	}); err != nil {
		t.Fatalf("Mark unread update: %v", err)
	}
	close(releaseCommit)
	<-done

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after stale event race: %v", err)
	}
	for _, label := range got.Labels {
		if label == "read" {
			t.Fatalf("labels after stale event race = %#v, want read removed", got.Labels)
		}
	}
	if got.Metadata["mail.read"] != "false" {
		t.Fatalf("mail.read after stale event race = %q, want false; metadata=%v", got.Metadata["mail.read"], got.Metadata)
	}
}

func TestCachingStoreApplyEventRechecksRecentLocalAfterGetRefresh(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	localTitle := "local"
	if err := cache.Update(bead.ID, UpdateOpts{Title: &localTitle}); err != nil {
		t.Fatalf("Update local title: %v", err)
	}
	cache.mu.Lock()
	cache.dirty[bead.ID] = struct{}{}
	cache.mu.Unlock()
	if _, err := cache.Get(bead.ID); err != nil {
		t.Fatalf("Get refresh after local update: %v", err)
	}

	cache.mu.RLock()
	_, locallyMutated := cache.beadSeq[bead.ID]
	recentlyLocal := recentLocalMutation(cache.localBeadAt[bead.ID], time.Now())
	cache.mu.RUnlock()
	if locallyMutated || !recentlyLocal {
		t.Fatalf("markers after Get refresh: locallyMutated=%v recentlyLocal=%v, want false/true", locallyMutated, recentlyLocal)
	}

	externalTitle := "external"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &externalTitle}); err != nil {
		t.Fatalf("Update backing external title: %v", err)
	}
	payload := json.RawMessage(fmt.Sprintf(`{"id":%q,"title":%q}`, bead.ID, externalTitle))

	beforeCommit := make(chan struct{})
	releaseCommit := make(chan struct{})
	cache.applyEventBeforeCommitForTest = func() {
		close(beforeCommit)
		<-releaseCommit
	}

	done := make(chan struct{})
	go func() {
		cache.ApplyEvent("bead.updated", payload)
		close(done)
	}()

	<-beforeCommit
	newerTitle := "newer local cache"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &newerTitle}); err != nil {
		t.Fatalf("Update backing newer title: %v", err)
	}
	cache.mu.Lock()
	cache.dirty[bead.ID] = struct{}{}
	cache.mu.Unlock()
	if _, err := cache.Get(bead.ID); err != nil {
		t.Fatalf("Get refresh before event commit: %v", err)
	}
	close(releaseCommit)
	<-done

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after stale event race: %v", err)
	}
	if got.Title != newerTitle {
		t.Fatalf("Title after stale event race = %q, want %q", got.Title, newerTitle)
	}
}

func TestCachingStoreApplyEventDropsRoutedEventOnConcurrentDepWrite(t *testing.T) {
	// Regression for gastownhall/gascity#2210 follow-up: the verified-backing
	// path applies a conflicting metadata event for a bead flagged locally
	// mutated only by a prior event. DepAdd/DepRemove mutate c.deps and bump the
	// mutation seq WITHOUT touching c.beads[id], so a concurrent dep write that
	// lands in the RUnlock->Lock window is invisible to the beadChanged guard
	// (which compares only the cached Bead) and gets clobbered by
	// updateEventDepsLocked. Snapshotting the mutation seq closes that hole: the
	// event must drop and let reconciliation reconverge, leaving the concurrent
	// dep write intact. Cover both the structured "dependencies" and the legacy
	// "needs" payload representations.
	cases := []struct {
		name         string
		created      Bead   // bead as the backing store first sees it (with deps)
		newDependsOn string // the dependency a concurrent DepAdd installs in the window
	}{
		{
			name:         "dependencies",
			created:      Bead{Title: "route me", Dependencies: []Dep{{DependsOnID: "dep-1", Type: "blocks"}}},
			newDependsOn: "dep-2",
		},
		{
			name:         "needs",
			created:      Bead{Title: "route me", Needs: []string{"need-1"}},
			newDependsOn: "need-2",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			backing := NewMemStore()

			cache := NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			// `gc bd create` writes the bead (with its deps) to the backing store
			// in another process; the controller learns it via a bead.created
			// event. Create after Prime so the apply sets the mutation seq
			// (locallyMutated) with no local write and seeds c.deps.
			created, err := backing.Create(tc.created)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			createdJSON, err := json.Marshal(created)
			if err != nil {
				t.Fatalf("marshal created: %v", err)
			}
			cache.ApplyEvent("bead.created", json.RawMessage(createdJSON))

			// `gc sling` stamps gc.routed_to in the backing store and emits a
			// bead.updated event carrying the full bead (same deps, now routed).
			if err := backing.SetMetadata(created.ID, "gc.routed_to", "pool/polecat"); err != nil {
				t.Fatalf("SetMetadata: %v", err)
			}
			routed, err := backing.Get(created.ID)
			if err != nil {
				t.Fatalf("Get backing: %v", err)
			}
			routedJSON, err := json.Marshal(routed)
			if err != nil {
				t.Fatalf("marshal routed: %v", err)
			}

			beforeCommit := make(chan struct{})
			releaseCommit := make(chan struct{})
			cache.applyEventBeforeCommitForTest = func() {
				close(beforeCommit)
				<-releaseCommit
			}

			done := make(chan struct{})
			go func() {
				cache.ApplyEvent("bead.updated", json.RawMessage(routedJSON))
				close(done)
			}()

			// A concurrent dep write lands after the routed event verified
			// against the backing store but before it commits.
			<-beforeCommit
			if err := cache.DepAdd(created.ID, tc.newDependsOn, "blocks"); err != nil {
				t.Fatalf("concurrent DepAdd: %v", err)
			}
			close(releaseCommit)
			<-done

			cache.mu.RLock()
			deps := cloneDeps(cache.deps[created.ID])
			cache.mu.RUnlock()

			found := false
			for _, d := range deps {
				if d.DependsOnID == tc.newDependsOn {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("concurrent dep %q clobbered by routed event; cached deps=%+v (event must drop and let reconciliation reconverge — #2210)",
					tc.newDependsOn, deps)
			}
		})
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

func TestCachingStoreRunReconciliationSuppressesDuplicateProblemLogs(t *testing.T) {
	t.Parallel()

	backing := &listFailingStore{Store: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "Task"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	var logs []string
	cache.problemf = func(msg string) {
		logs = append(logs, msg)
	}

	backing.failList = true
	for i := 0; i < maxCacheSyncFailures; i++ {
		cache.runReconciliation()
	}

	stats := cache.Stats()
	if stats.ProblemCount != int64(maxCacheSyncFailures) {
		t.Fatalf("ProblemCount = %d, want %d", stats.ProblemCount, maxCacheSyncFailures)
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d problem lines, want 1: %#v", len(logs), logs)
	}
	if delay := cache.nextReconcileDelay(time.Now()); delay <= cacheReconcilePollInterval {
		t.Fatalf("nextReconcileDelay = %v, want sustained-failure backoff above poll interval", delay)
	}

	cache.mu.Lock()
	state := cache.problemLog[stats.LastProblem]
	state.lastAt = time.Now().Add(-cacheProblemLogWindow)
	cache.problemLog[stats.LastProblem] = state
	cache.mu.Unlock()

	cache.runReconciliation()
	if len(logs) != 2 {
		t.Fatalf("logged %d problem lines after window expiry, want 2: %#v", len(logs), logs)
	}
	if !strings.Contains(logs[1], "suppressed 4 duplicate logs") {
		t.Fatalf("second problem log = %q, want suppressed duplicate count", logs[1])
	}
}

func TestCachingStorePrimeActiveUsesPartialResultRows(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:           NewMemStore(),
		partialStatuses: map[string]bool{"open": true},
	}
	open, err := backing.Create(Bead{Title: "open survivor"})
	if err != nil {
		t.Fatalf("Create(open): %v", err)
	}
	inProgress, err := backing.Create(Bead{Title: "in progress survivor"})
	if err != nil {
		t.Fatalf("Create(in_progress): %v", err)
	}
	status := "in_progress"
	if err := backing.Update(inProgress.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update(in_progress): %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	cache.mu.RLock()
	_, hasOpen := cache.beads[open.ID]
	_, hasInProgress := cache.beads[inProgress.ID]
	cache.mu.RUnlock()
	if !hasOpen || !hasInProgress {
		t.Fatalf("cache.beads has open=%v in_progress=%v, want both partial rows retained", hasOpen, hasInProgress)
	}
	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "prime active (open)") {
		t.Fatalf("LastProblem = %q, want prime active context", stats.LastProblem)
	}
	if cache.state != cachePartial {
		t.Fatalf("state = %v, want cachePartial", cache.state)
	}
}

func TestCachingStorePrimeUsesPartialResultRows(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "prime survivor"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.mu.RLock()
	_, hasSurvivor := cache.beads[survivor.ID]
	cache.mu.RUnlock()
	if !hasSurvivor {
		t.Fatalf("cache.beads missing partial survivor %s", survivor.ID)
	}
	stats := cache.Stats()
	if stats.ProblemCount != 1 {
		t.Fatalf("ProblemCount = %d, want 1", stats.ProblemCount)
	}
	if !strings.Contains(stats.LastProblem, "prime cache") {
		t.Fatalf("LastProblem = %q, want prime cache context", stats.LastProblem)
	}
	if cache.state != cacheLive {
		t.Fatalf("state = %v, want cacheLive", cache.state)
	}
}

func TestCachingStoreCachedListRejectsPartialPrime(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "survives partial prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	if _, err := backing.Create(Bead{Title: "dropped by bd parse"}); err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	items, ok := cache.CachedList(ListQuery{AllowScan: true})
	if ok {
		t.Fatalf("CachedList ok = true with items %+v, want ok=false while primePartialErr is set", items)
	}
}

func TestCachingStorePrimePartialDoesNotServeActiveListAsComplete(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "survives partial prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped by bd parse"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	items, err := cache.List(ListQuery{AllowScan: true})
	var partial *PartialResultError
	if !errors.As(err, &partial) {
		t.Fatalf("List() error = %v, want *PartialResultError after partial prime", err)
	}
	if hasBead(items, dropped.ID) {
		t.Fatalf("List() returned dropped bead %s despite backing partial rows: %+v", dropped.ID, items)
	}
	if !hasBead(items, survivor.ID) {
		t.Fatalf("List() = %+v, want partial survivor %s", items, survivor.ID)
	}
}

func TestCachingStorePrimeActivePartialFallsBackForActiveList(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:           NewMemStore(),
		partialStatuses: map[string]bool{"open": true},
	}
	survivor, err := backing.Create(Bead{Title: "survives partial active prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped from primed status"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	items, err := cache.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List() error = %v, want clean backing fallback", err)
	}
	if !hasBead(items, survivor.ID) || !hasBead(items, dropped.ID) {
		t.Fatalf("List() = %+v, want backing fallback to return survivor %s and dropped %s", items, survivor.ID, dropped.ID)
	}
}

func TestCachingStoreReadyFallsBackAfterPartialPrime(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{
		Store:            NewMemStore(),
		partialAllowScan: true,
	}
	survivor, err := backing.Create(Bead{Title: "survives partial prime"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped by bd parse"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	backing.partialRows = []Bead{survivor}

	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	items, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready() error = %v, want backing fallback success", err)
	}
	if !hasBead(items, survivor.ID) || !hasBead(items, dropped.ID) {
		t.Fatalf("Ready() = %+v, want backing fallback to include survivor %s and dropped %s", items, survivor.ID, dropped.ID)
	}
}

func TestCachingStoreRunReconciliationDoesNotTreatPartialResultAsAuthoritative(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{Store: NewMemStore()}
	survivor, err := backing.Create(Bead{Title: "survives partial list"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	dropped, err := backing.Create(Bead{Title: "dropped by bd parse"})
	if err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.partialAllowScan = true
	claimedStatus := "in_progress"
	if err := backing.Update(survivor.ID, UpdateOpts{Status: &claimedStatus}); err != nil {
		t.Fatalf("Update(survivor): %v", err)
	}
	updatedSurvivor, err := backing.Get(survivor.ID)
	if err != nil {
		t.Fatalf("Get(updated survivor): %v", err)
	}
	backing.partialRows = []Bead{updatedSurvivor}
	cache.runReconciliation()
	for i := 1; i < maxCacheSyncFailures; i++ {
		cache.runReconciliation()
	}

	for _, event := range events {
		if event == "bead.closed:"+dropped.ID {
			t.Fatalf("partial reconcile emitted synthetic close for dropped row: %v", events)
		}
		if event == "bead.updated:"+survivor.ID {
			t.Fatalf("partial reconcile emitted update for survivor row: %v", events)
		}
	}
	cache.mu.RLock()
	_, stillCached := cache.beads[dropped.ID]
	cachedSurvivor := cache.beads[survivor.ID]
	state := cache.state
	syncFailures := cache.syncFailures
	cache.mu.RUnlock()
	if !stillCached {
		t.Fatalf("dropped row %s was evicted from cache after partial reconcile", dropped.ID)
	}
	if cachedSurvivor.Status == claimedStatus {
		t.Fatalf("survivor status = %q, want partial reconcile to leave cached status non-authoritative", cachedSurvivor.Status)
	}
	if state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after repeated partial list failures", state)
	}
	if syncFailures != maxCacheSyncFailures {
		t.Fatalf("syncFailures = %d, want %d", syncFailures, maxCacheSyncFailures)
	}
	stats := cache.Stats()
	if stats.ProblemCount != int64(maxCacheSyncFailures) {
		t.Fatalf("ProblemCount = %d, want %d", stats.ProblemCount, maxCacheSyncFailures)
	}
}

func TestCachingStoreRunReconciliationDegradesImmediatelyOnPartialResult(t *testing.T) {
	t.Parallel()

	backing := &readyCountingPartialListStore{
		partialListErrorStore: &partialListErrorStore{
			Store:           NewMemStore(),
			partialStatuses: map[string]bool{"open": true},
		},
	}
	survivor, err := backing.Create(Bead{Title: "survives partial list"})
	if err != nil {
		t.Fatalf("Create(survivor): %v", err)
	}
	if _, err := backing.Create(Bead{Title: "dropped by bd parse"}); err != nil {
		t.Fatalf("Create(dropped): %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.partialAllowScan = true
	backing.partialRows = []Bead{survivor}
	cache.runReconciliation()

	cache.mu.RLock()
	state := cache.state
	cache.mu.RUnlock()
	if state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after one partial reconcile", state)
	}
	items, err := cache.List(ListQuery{Status: "open"})
	if !IsPartialResult(err) {
		t.Fatalf("List() error = %v, want PartialResultError", err)
	}
	if !hasBead(items, survivor.ID) {
		t.Fatalf("List() = %+v, want survivor %s from backing fallback", items, survivor.ID)
	}
	if cached, ok := cache.CachedList(ListQuery{Status: "open"}); ok {
		t.Fatalf("CachedList() = %+v, true; want unavailable after partial reconcile", cached)
	}
	readyCalls := backing.readyCalls
	if _, err := cache.Ready(); err != nil {
		t.Fatalf("Ready(): %v", err)
	}
	if backing.readyCalls == readyCalls {
		t.Fatalf("Ready() did not fall back to backing store after partial reconcile")
	}
}

func TestCachingStoreRunReconciliationDegradesPartialCache(t *testing.T) {
	t.Parallel()

	backing := &partialListErrorStore{Store: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "active bead"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	backing.partialAllowScan = true
	for i := 0; i < maxCacheSyncFailures; i++ {
		cache.runReconciliation()
	}

	cache.mu.RLock()
	state := cache.state
	syncFailures := cache.syncFailures
	cache.mu.RUnlock()
	if state != cacheDegraded {
		t.Fatalf("state = %v, want cacheDegraded after repeated partial reconcile failures from cachePartial", state)
	}
	if syncFailures != maxCacheSyncFailures {
		t.Fatalf("syncFailures = %d, want %d", syncFailures, maxCacheSyncFailures)
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

func TestCachingStoreCachedListUnavailableAfterWriteThroughRefreshFailure(t *testing.T) {
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

	if rows, ok := cache.CachedList(ListQuery{Status: "open"}); ok {
		t.Fatalf("CachedList returned clean rows after refresh failure: %#v", rows)
	}
	if _, err := cache.Handles().Cached.List(ListQuery{Status: "open"}); !errors.Is(err, ErrCacheUnavailable) {
		t.Fatalf("Cached.List after refresh failure = %v, want ErrCacheUnavailable", err)
	}

	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after refresh failure: %v", err)
	}
	if got.Title != title {
		t.Fatalf("Get title = %q, want authoritative title %q", got.Title, title)
	}
	rows, ok := cache.CachedList(ListQuery{Status: "open"})
	if !ok {
		t.Fatal("CachedList returned ok=false after authoritative Get refresh")
	}
	if len(rows) != 1 || rows[0].ID != bead.ID || rows[0].Title != title {
		t.Fatalf("CachedList after authoritative refresh = %#v, want %s title %q", rows, bead.ID, title)
	}
}

func TestCachingStoreReconciliationClearsDirtyWriteThroughProjection(t *testing.T) {
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
	if rows, ok := cache.CachedList(ListQuery{Status: "open"}); ok {
		t.Fatalf("CachedList returned clean rows after refresh failure: %#v", rows)
	}

	cache.runReconciliation()

	rows, ok := cache.CachedList(ListQuery{Status: "open"})
	if !ok {
		t.Fatal("CachedList returned ok=false after authoritative reconciliation")
	}
	if len(rows) != 1 || rows[0].ID != bead.ID || rows[0].Title != title {
		t.Fatalf("CachedList after reconciliation = %#v, want %s title %q", rows, bead.ID, title)
	}
}

func TestCachingStoreCachedListSupportsActiveTierQueries(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	plain, err := backing.Create(Bead{Title: "plain", Labels: []string{"k"}})
	if err != nil {
		t.Fatalf("Create plain: %v", err)
	}
	wisp, err := backing.Create(Bead{Title: "wisp", Labels: []string{"k"}, Ephemeral: true})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	wisps, ok := cache.CachedList(ListQuery{Label: "k", TierMode: TierWisps})
	if !ok {
		t.Fatal("CachedList wisps ok=false, want cached result")
	}
	if len(wisps) != 1 || wisps[0].ID != wisp.ID {
		t.Fatalf("CachedList wisps = %#v, want %s", wisps, wisp.ID)
	}
	both, ok := cache.CachedList(ListQuery{Label: "k", TierMode: TierBoth})
	if !ok {
		t.Fatal("CachedList both ok=false, want cached result")
	}
	ids := map[string]bool{}
	for _, row := range both {
		ids[row.ID] = true
	}
	if len(both) != 2 || !ids[plain.ID] || !ids[wisp.ID] {
		t.Fatalf("CachedList both ids = %v rows=%#v, want %s and %s", ids, both, plain.ID, wisp.ID)
	}
}

func TestCachingStoreCachedListRejectsIncludeClosedQueries(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	if _, err := backing.Create(Bead{Title: "order run", Labels: []string{"order-run:daily"}, Ephemeral: true}); err != nil {
		t.Fatalf("Create order run: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	rows, ok := cache.CachedList(ListQuery{
		Label:         "order-run:daily",
		IncludeClosed: true,
		TierMode:      TierBoth,
		Limit:         1,
	})
	if ok {
		t.Fatalf("CachedList IncludeClosed ok=true rows=%#v, want ok=false", rows)
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

type deleteAfterUpdateStore struct {
	Store
}

func (s *deleteAfterUpdateStore) Update(id string, opts UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	return s.Delete(id)
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

type partialListErrorStore struct {
	Store
	partialStatuses  map[string]bool
	partialAllowScan bool
	partialRows      []Bead
}

func (s *partialListErrorStore) List(query ListQuery) ([]Bead, error) {
	items, err := s.Store.List(query)
	if err != nil {
		return nil, err
	}
	if s.partialStatuses[query.Status] || (s.partialAllowScan && query.AllowScan) {
		if s.partialRows != nil {
			items = make([]Bead, len(s.partialRows))
			for i := range s.partialRows {
				items[i] = cloneBead(s.partialRows[i])
			}
		}
		return items, &PartialResultError{
			Op:  "bd list",
			Err: errors.New("skipped 1 corrupt bead"),
		}
	}
	return items, nil
}

type readyCountingPartialListStore struct {
	*partialListErrorStore
	readyCalls int
}

func (s *readyCountingPartialListStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	s.readyCalls++
	return s.partialListErrorStore.Ready(query...)
}

func hasBead(items []Bead, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
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

func TestCachingStoreBdPrimeAndReconcileSkipFullDepScan(t *testing.T) {
	t.Parallel()

	var depListCalls int
	var readyCalls int
	issueJSON := []byte(`[{
		"id":"bd-1",
		"title":"task",
		"status":"open",
		"issue_type":"task",
		"created_at":"2026-01-01T00:00:00Z",
		"labels":["task"],
		"metadata":{}
	}]`)
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			t.Fatalf("command name = %q, want bd", name)
		}
		if len(args) > 0 && args[0] == "dep" {
			depListCalls++
			t.Fatalf("unexpected dep scan command: %v", args)
		}
		if len(args) > 0 && args[0] == "ready" {
			readyCalls++
			return issueJSON, nil
		}
		if len(args) > 0 && args[0] == "list" {
			return issueJSON, nil
		}
		return []byte(`[]`), nil
	}
	cache := NewCachingStore(NewBdStore("/city", runner), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.runReconciliation()
	if depListCalls != 0 {
		t.Fatalf("dep list calls = %d, want 0", depListCalls)
	}
	if _, err := cache.Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if readyCalls != 1 {
		t.Fatalf("Ready calls = %d, want backing Ready fallback when deps are incomplete", readyCalls)
	}
}

func TestCachingStoreBdPrimeActiveUsesListDependenciesForCachedReady(t *testing.T) {
	t.Parallel()

	var depListCalls int
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			t.Fatalf("command name = %q, want bd", name)
		}
		if len(args) > 0 && args[0] == "dep" {
			depListCalls++
			t.Fatalf("unexpected dep scan command: %v", args)
		}
		if len(args) > 0 && args[0] == "list" {
			argLine := strings.Join(args, " ")
			if strings.Contains(argLine, "--status=open") {
				return []byte(`[
					{"id":"bd-blocker","title":"blocker","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","labels":["task"],"metadata":{}},
					{"id":"bd-blocked","title":"blocked","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:01Z","labels":["task"],"metadata":{},"dependencies":[{"issue_id":"bd-blocked","depends_on_id":"bd-blocker","type":"blocks"}]}
				]`), nil
			}
			if strings.Contains(argLine, "--status=in_progress") {
				return []byte(`[]`), nil
			}
		}
		return []byte(`[]`), nil
	}
	cache := NewCachingStoreForTest(NewBdStore("/city", runner), nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	ready, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable")
	}
	ids := map[string]bool{}
	for _, b := range ready {
		ids[b.ID] = true
	}
	if !ids["bd-blocker"] || ids["bd-blocked"] {
		t.Fatalf("CachedReady ids = %v, want blocker ready and blocked excluded", ids)
	}
	if depListCalls != 0 {
		t.Fatalf("dep list calls = %d, want 0", depListCalls)
	}
}

func TestCachingStoreReadySkipsEphemeralOpenTasks(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	ready, err := cache.Create(Bead{Title: "ready", Type: "task"})
	if err != nil {
		t.Fatalf("Create ready: %v", err)
	}
	ephemeral, err := cache.Create(Bead{Title: "tracking", Type: "task", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create ephemeral: %v", err)
	}

	got, err := cache.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != ready.ID {
		t.Fatalf("Ready() = %+v, want only non-ephemeral task %s", got, ready.ID)
	}
	cached, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable")
	}
	if len(cached) != 1 || cached[0].ID != ready.ID {
		t.Fatalf("CachedReady() = %+v, want only non-ephemeral task %s", cached, ready.ID)
	}
	for _, bead := range append(got, cached...) {
		if bead.ID == ephemeral.ID {
			t.Fatalf("ephemeral bead %s leaked into cached ready paths", ephemeral.ID)
		}
	}
}

func TestCachingStoreBdReconcileRefreshesListDependenciesForCachedReady(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	assertCachedReadyContains := func(wantReady bool) {
		t.Helper()
		ready, ok := cache.CachedReady()
		if !ok {
			t.Fatal("CachedReady reported cache unavailable")
		}
		readyByID := make(map[string]bool, len(ready))
		for _, bead := range ready {
			readyByID[bead.ID] = true
		}
		if readyByID["bd-1"] != wantReady {
			t.Fatalf("CachedReady includes bd-1 = %v, want %v; ready=%v", readyByID["bd-1"], wantReady, readyByID)
		}
	}

	assertCachedReadyContains(true)

	runner.deps["bd-1"] = []Dep{{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"}}
	cache.runReconciliation()
	assertCachedReadyContains(false)

	runner.deps["bd-1"] = nil
	cache.runReconciliation()
	assertCachedReadyContains(true)

	if runner.depScanCalls != 0 {
		t.Fatalf("dep scan calls = %d, want 0", runner.depScanCalls)
	}
}

func TestCachingStoreBdReconcileClearsCachedDepsWhenListOmitsDependencies(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	runner.deps["bd-1"] = []Dep{{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"}}
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	runner.deps["bd-1"] = nil
	cache.runReconciliation()

	ready, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported cache unavailable")
	}
	readyByID := make(map[string]bool, len(ready))
	for _, bead := range ready {
		readyByID[bead.ID] = true
	}
	if !readyByID["bd-1"] {
		t.Fatalf("CachedReady excludes bd-1 after omitted deps, ready=%v", readyByID)
	}
}

func TestCachingStoreBdIncompleteDepsUseBackingForDownDepList(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	deps, err := cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("initial DepList: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("initial deps = %v, want empty", deps)
	}

	runner.deps["bd-1"] = []Dep{{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"}}
	cache.runReconciliation()

	deps, err = cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("DepList after external dep add: %v", err)
	}
	if !hasDep(deps, "bd-2") {
		t.Fatalf("deps after external dep add = %v, want bd-1 -> bd-2 from backing store", deps)
	}
	if runner.depScanCalls != 0 {
		t.Fatalf("dep scan calls = %d, want 0", runner.depScanCalls)
	}
}

func TestCachingStoreCompleteEmbeddedDepsAvoidPerIDDepList(t *testing.T) {
	t.Parallel()

	backing := &completeEmbeddedDepsStore{
		Store: NewMemStore(),
		beads: []Bead{
			{ID: "gc-parent", Title: "parent", Status: "open", Type: "task"},
			{
				ID:     "gc-child",
				Title:  "child",
				Status: "open",
				Type:   "task",
				Dependencies: []Dep{{
					IssueID:     "gc-child",
					DependsOnID: "gc-parent",
					Type:        "blocks",
				}},
			},
		},
	}
	cache := NewCachingStore(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	deps, err := cache.DepList("gc-child", "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].IssueID != "gc-child" || deps[0].DependsOnID != "gc-parent" || deps[0].Type != "blocks" {
		t.Fatalf("deps = %v, want embedded gc-child -> gc-parent", deps)
	}
	if backing.depListCalls != 0 {
		t.Fatalf("backing DepList calls = %d, want 0", backing.depListCalls)
	}
}

func TestCachingStoreBdIncompleteDepsDepAddDoesNotDropExistingBackingDeps(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	runner.deps["bd-1"] = []Dep{{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"}}
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if err := cache.DepAdd("bd-1", "bd-3", "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	deps, err := cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("DepList after DepAdd: %v", err)
	}
	if !hasDep(deps, "bd-2") || !hasDep(deps, "bd-3") {
		t.Fatalf("deps after DepAdd = %v, want existing bd-2 and added bd-3", deps)
	}
}

func TestCachingStoreBdIncompleteDepsDepRemoveDoesNotDropExternalBackingDeps(t *testing.T) {
	t.Parallel()

	runner := newCachingStoreBdDepRunner(t)
	runner.deps["bd-1"] = []Dep{
		{IssueID: "bd-1", DependsOnID: "bd-2", Type: "blocks"},
		{IssueID: "bd-1", DependsOnID: "bd-3", Type: "blocks"},
	}
	cache := NewCachingStore(NewBdStore("/city", runner.run), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, err := cache.DepList("bd-1", "down"); err != nil {
		t.Fatalf("DepList before external add: %v", err)
	}
	runner.deps["bd-1"] = append(runner.deps["bd-1"], Dep{IssueID: "bd-1", DependsOnID: "bd-4", Type: "blocks"})

	if err := cache.DepRemove("bd-1", "bd-3"); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}

	deps, err := cache.DepList("bd-1", "down")
	if err != nil {
		t.Fatalf("DepList after DepRemove: %v", err)
	}
	if hasDep(deps, "bd-3") {
		t.Fatalf("deps after DepRemove = %v, still contains removed bd-3", deps)
	}
	if !hasDep(deps, "bd-2") || !hasDep(deps, "bd-4") {
		t.Fatalf("deps after DepRemove = %v, want retained bd-2 and external bd-4", deps)
	}
}

type completeEmbeddedDepsStore struct {
	Store
	beads        []Bead
	depListCalls int
}

func (s *completeEmbeddedDepsStore) listIncludesCompleteDependencies() bool {
	return true
}

func (s *completeEmbeddedDepsStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	items := make([]Bead, 0, len(s.beads))
	for _, b := range s.beads {
		items = append(items, cloneBead(b))
	}
	return ApplyListQuery(items, query), nil
}

func (s *completeEmbeddedDepsStore) DepList(string, string) ([]Dep, error) {
	s.depListCalls++
	return nil, errors.New("unexpected per-ID DepList")
}

type cachingStoreBdDepRunner struct {
	t            *testing.T
	deps         map[string][]Dep
	depScanCalls int
}

func newCachingStoreBdDepRunner(t *testing.T) *cachingStoreBdDepRunner {
	t.Helper()
	return &cachingStoreBdDepRunner{
		t:    t,
		deps: make(map[string][]Dep),
	}
}

func (r *cachingStoreBdDepRunner) run(_, name string, args ...string) ([]byte, error) {
	r.t.Helper()
	if name != "bd" {
		r.t.Fatalf("command name = %q, want bd", name)
	}
	if len(args) == 0 {
		r.t.Fatal("empty bd command")
	}
	switch args[0] {
	case "list":
		return r.listOutput(), nil
	case "ready":
		return []byte(`[]`), nil
	case "dep":
		return r.runDep(args[1:]...)
	default:
		return []byte(`[]`), nil
	}
}

func (r *cachingStoreBdDepRunner) runDep(args ...string) ([]byte, error) {
	r.t.Helper()
	if len(args) == 0 {
		r.t.Fatal("empty bd dep command")
	}
	switch args[0] {
	case "list":
		if len(args) > 1 && args[1] == "bd-1" {
			return r.depListOutput("bd-1"), nil
		}
		r.depScanCalls++
		r.t.Fatalf("unexpected dep scan command: %v", args)
	case "add":
		if len(args) < 5 || args[3] != "--type" {
			r.t.Fatalf("unexpected dep add args: %v", args)
		}
		r.addDep(args[1], args[2], args[4])
		return []byte(`[]`), nil
	case "remove":
		if len(args) < 3 {
			r.t.Fatalf("unexpected dep remove args: %v", args)
		}
		r.removeDep(args[1], args[2])
		return []byte(`[]`), nil
	}
	r.t.Fatalf("unexpected dep command: %v", args)
	return nil, nil
}

func (r *cachingStoreBdDepRunner) listOutput() []byte {
	var b strings.Builder
	ids := []string{"bd-1", "bd-2", "bd-3", "bd-4"}
	b.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q,"title":%q,"status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","labels":["task"],"metadata":{}`, id, "dep "+strings.TrimPrefix(id, "bd-"))
		if deps := r.deps[id]; len(deps) > 0 {
			b.WriteString(`,"dependencies":[`)
			for depIdx, dep := range deps {
				if depIdx > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"issue_id":%q,"depends_on_id":%q,"type":%q}`, dep.IssueID, dep.DependsOnID, dep.Type)
			}
			b.WriteByte(']')
		}
		b.WriteByte('}')
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func (r *cachingStoreBdDepRunner) depListOutput(issueID string) []byte {
	deps := r.deps[issueID]
	if len(deps) == 0 {
		return []byte(`[]`)
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, dep := range deps {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q,"title":"dep","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z","dependency_type":%q}`, dep.DependsOnID, dep.Type)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func (r *cachingStoreBdDepRunner) addDep(issueID, dependsOnID, depType string) {
	deps := r.deps[issueID]
	for i, dep := range deps {
		if dep.DependsOnID == dependsOnID {
			deps[i].Type = depType
			r.deps[issueID] = deps
			return
		}
	}
	r.deps[issueID] = append(deps, Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
}

func (r *cachingStoreBdDepRunner) removeDep(issueID, dependsOnID string) {
	deps := r.deps[issueID]
	for i, dep := range deps {
		if dep.DependsOnID == dependsOnID {
			r.deps[issueID] = append(deps[:i], deps[i+1:]...)
			return
		}
	}
}

func hasDep(deps []Dep, dependsOnID string) bool {
	for _, dep := range deps {
		if dep.IssueID == "bd-1" && dep.DependsOnID == dependsOnID {
			return true
		}
	}
	return false
}
