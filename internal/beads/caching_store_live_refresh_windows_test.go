//go:build windows

package beads

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

type cacheEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *cacheEventRecorder) callback(eventType, beadID string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventType+":"+beadID)
}

func (r *cacheEventRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}

func (r *cacheEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *cacheEventRecorder) count(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, got := range r.events {
		if got == event {
			n++
		}
	}
	return n
}

func requireCloseCount(t *testing.T, r *cacheEventRecorder, id string, want int) {
	t.Helper()
	event := "bead.closed:" + id
	if got := r.count(event); got != want {
		t.Fatalf("close notifications for %s = %d in %v, want %d", id, got, r.snapshot(), want)
	}
}

func requireEventSequence(t *testing.T, r *cacheEventRecorder, want ...string) {
	t.Helper()
	got := r.snapshot()
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func fix7rReviewCache(t *testing.T, backing Store) (*CachingStore, *cacheEventRecorder) {
	t.Helper()
	r := &cacheEventRecorder{}
	c := NewCachingStoreForTest(backing, r.callback)
	if err := c.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, r
}

type fix7rLostCloseReplyStore struct {
	Store
	after func()
}

func (s *fix7rLostCloseReplyStore) Close(id string) error {
	if err := s.Store.Close(id); err != nil {
		return err
	}
	if s.after != nil {
		s.after()
	}
	return errors.New("close response lost")
}

func TestFailedCloseRetainsObservedNotification(t *testing.T) {
	backing := &fix7rLostCloseReplyStore{
		Store: NewMemStoreFrom(1, []Bead{{ID: "a", Status: "open"}}, nil),
	}
	cache, events := fix7rReviewCache(t, backing)
	backing.after = func() {
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatal(err)
		}
	}

	events.reset()
	if err := cache.Close("a"); err == nil {
		t.Fatal("expected lost close response")
	}
	cache.runReconciliation()
	requireCloseCount(t, events, "a", 1)
}

type failingCloseGateStore struct {
	Store
	entered chan struct{}
	release chan struct{}
}

func (s *failingCloseGateStore) Close(string) error {
	close(s.entered)
	<-s.release
	return errors.New("injected close failure")
}

func TestFailedLocalCloseDoesNotSuppressConcurrentForeignClose(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gate := &failingCloseGateStore{
		Store:   mem,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(gate, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	events.reset()
	result := make(chan error, 1)
	go func() {
		result <- cache.Close(bead.ID)
	}()
	<-gate.entered
	if err := mem.Close(bead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	if err := <-result; err == nil {
		t.Fatal("expected local close failure")
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

func TestCloseIfMatchRecloseCachedClosedStillReports(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reclose"})
	if err != nil {
		t.Fatal(err)
	}
	cache, events := fix7rReviewCache(t, backing)
	if err := cache.Close(bead.ID); err != nil {
		t.Fatal(err)
	}
	closed, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}

	events.reset()
	if err := cache.CloseIfMatch(bead.ID, closed.Revision); err != nil {
		t.Fatal(err)
	}
	requireCloseCount(t, events, bead.ID, 1)
}

func TestLocalCloseAfterForeignReopenStillNotifies(t *testing.T) {
	t.Parallel()

	for _, method := range []string{"conditional", "atomic", "tx"} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			backing := NewAtomicCloseMemStore()
			bead, err := backing.Create(Bead{Title: "two lifecycles"})
			if err != nil {
				t.Fatal(err)
			}
			cache, events := fix7rReviewCache(t, backing)
			if err := cache.Close(bead.ID); err != nil {
				t.Fatal(err)
			}
			if err := backing.Reopen(bead.ID); err != nil {
				t.Fatal(err)
			}
			current, err := backing.Get(bead.ID)
			if err != nil {
				t.Fatal(err)
			}

			events.reset()
			switch method {
			case "conditional":
				err = cache.CloseIfMatch(bead.ID, current.Revision)
			case "atomic":
				closer, ok := AtomicConditionalCloserFor(cache)
				if !ok {
					t.Fatal("atomic closer unavailable")
				}
				_, err = closer.CloseWithMetadataIfMatch(bead.ID, current.Revision, map[string]string{"state": "done"})
			case "tx":
				err = cache.Tx("close new lifecycle", func(tx Tx) error {
					return tx.Close(bead.ID)
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			cache.runReconciliation()
			requireCloseCount(t, events, bead.ID, 1)
		})
	}
}

func TestTxCloseWithConcurrentLiveReadReportsCloseOnce(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "tx close"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	events.reset()
	if err := cache.Tx("close", func(tx Tx) error {
		if err := tx.Close(bead.ID); err != nil {
			return err
		}
		_, err := cache.List(ListQuery{Status: "open", Live: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

func TestTxCloseThenLiveReadReportsCloseOnce(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "tx close"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	events.reset()
	if err := cache.Tx("close", func(tx Tx) error {
		return tx.Close(bead.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

type conditionalCloseAfterStore struct {
	Store
	after func()
}

func (s *conditionalCloseAfterStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	writer, ok := ConditionalWriterFor(s.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	return writer.UpdateIfMatch(id, expectedRevision, opts)
}

func (s *conditionalCloseAfterStore) CloseIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(s.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	if err := writer.CloseIfMatch(id, expectedRevision); err != nil {
		return err
	}
	if s.after != nil {
		s.after()
	}
	return nil
}

func (s *conditionalCloseAfterStore) DeleteIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(s.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	return writer.DeleteIfMatch(id, expectedRevision)
}

func (s *conditionalCloseAfterStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := ConditionalWriterFor(s.Store)
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	return writer.CompareAndSetMetadataKey(id, key, expected, next)
}

func TestCloseIfMatchWithConcurrentLiveReadReportsCloseOnce(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "conditional close"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &conditionalCloseAfterStore{Store: mem}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	backing.after = func() {
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatal(err)
		}
	}

	events.reset()
	if err := cache.CloseIfMatch(bead.ID, got.Revision); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

func TestCloseIfMatchThenLiveReadReportsCloseOnce(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "conditional close"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}

	events.reset()
	if err := cache.CloseIfMatch(bead.ID, got.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

type atomicCloseAfterStore struct {
	Store
	after func()
}

func (s *atomicCloseAfterStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	closer, ok := AtomicConditionalCloserFor(s.Store)
	if !ok {
		return Bead{}, ErrConditionalWriteUnsupported
	}
	closed, err := closer.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
	if err != nil {
		return Bead{}, err
	}
	if s.after != nil {
		s.after()
	}
	return closed, nil
}

func TestAtomicConditionalCloseWithConcurrentLiveReadReportsCloseOnce(t *testing.T) {
	t.Parallel()

	backing := &atomicCloseAfterStore{Store: NewAtomicCloseMemStore()}
	bead, err := backing.Create(Bead{Title: "atomic close"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	backing.after = func() {
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatal(err)
		}
	}
	closer, ok := AtomicConditionalCloserFor(cache)
	if !ok {
		t.Fatal("AtomicConditionalCloserFor(cache) unavailable")
	}

	events.reset()
	if _, err := closer.CloseWithMetadataIfMatch(bead.ID, got.Revision, map[string]string{"state": "done"}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

func TestAtomicConditionalCloseThenLiveReadReportsCloseOnce(t *testing.T) {
	t.Parallel()

	backing := NewAtomicCloseMemStore()
	bead, err := backing.Create(Bead{Title: "atomic close"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	closer, ok := AtomicConditionalCloserFor(cache)
	if !ok {
		t.Fatal("AtomicConditionalCloserFor(cache) unavailable")
	}

	events.reset()
	if _, err := closer.CloseWithMetadataIfMatch(bead.ID, got.Revision, map[string]string{"state": "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

type afterScanStore struct {
	*MemStore
	afterScan func()
}

func (s *afterScanStore) List(query ListQuery) ([]Bead, error) {
	out, err := s.MemStore.List(query)
	if query.AllowScan && s.afterScan != nil {
		after := s.afterScan
		s.afterScan = nil
		after()
	}
	return out, err
}

type fix7r3StaleLiveListStore struct {
	*MemStore
	afterLiveList func()
}

func (s *fix7r3StaleLiveListStore) List(q ListQuery) ([]Bead, error) {
	out, err := s.MemStore.List(q)
	if q.Live && s.afterLiveList != nil {
		hook := s.afterLiveList
		s.afterLiveList = nil
		hook()
	}
	return out, err
}

type lagAfterCloseList struct {
	*MemStore
	lagged map[string]bool
}

func (s *lagAfterCloseList) Close(id string) error {
	if err := s.MemStore.Close(id); err != nil {
		return err
	}
	if s.lagged == nil {
		s.lagged = make(map[string]bool)
	}
	s.lagged[id] = true
	return nil
}

func (s *lagAfterCloseList) List(q ListQuery) ([]Bead, error) {
	items, err := s.MemStore.List(q)
	if err != nil {
		return nil, err
	}
	for id := range s.lagged {
		b, err := s.MemStore.Get(id)
		if err != nil {
			continue
		}
		b.Status = "in_progress"
		if q.Matches(b) {
			items = append(items, b)
		}
	}
	return items, nil
}

func TestAgedLocalCloseSurvivesLaggedLiveListWithoutInterveningMutation(t *testing.T) {
	mem := NewMemStore()
	x, err := mem.Create(Bead{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	st := "in_progress"
	if err := mem.Update(x.ID, UpdateOpts{Status: &st}); err != nil {
		t.Fatal(err)
	}
	cache, _ := fix7rReviewCache(t, &lagAfterCloseList{MemStore: mem})
	if err := cache.Close(x.ID); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	cache.localBeadAt[x.ID] = time.Now().Add(-time.Minute)
	cache.mu.Unlock()
	if _, err := cache.List(ListQuery{Status: "in_progress", Live: true}); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}

func TestStaleLiveReadDoesNotReopenReadReportedClose(t *testing.T) {
	mem := NewMemStore()
	x, err := mem.Create(Bead{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &fix7r3StaleLiveListStore{MemStore: mem}
	cache, events := fix7rReviewCache(t, backing)
	if _, err := cache.Create(Bead{Title: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	events.reset()
	backing.afterLiveList = func() {
		if err := mem.Close(x.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, x.ID, 1)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	cache.runReconciliation()
	requireCloseCount(t, events, x.ID, 1)
}

func TestOlderLiveReadDoesNotUndoReportedClose(t *testing.T) {
	backing := &afterScanStore{MemStore: NewMemStoreFrom(1, []Bead{{ID: "a", Status: "open"}}, nil)}
	cache, events := fix7rReviewCache(t, backing)
	if _, err := cache.Create(Bead{Title: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	events.reset()
	backing.afterScan = func() {
		if err := backing.Close("a"); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "closed", Live: true}); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, "a", 1)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true, AllowScan: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "closed", Live: true}); err != nil {
		t.Fatal(err)
	}
	requireCloseCount(t, events, "a", 1)
}

type fix7r3StaleGetStore struct {
	*MemStore
	targetID string
	afterGet func()
}

func (s *fix7r3StaleGetStore) Get(id string) (Bead, error) {
	out, err := s.MemStore.Get(id)
	if id == s.targetID && s.afterGet != nil {
		hook := s.afterGet
		s.afterGet = nil
		hook()
	}
	return out, err
}

func TestStaleDirtyGetDoesNotReopenReadReportedClose(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "dirty get"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &fix7r3StaleGetStore{MemStore: mem, targetID: bead.ID}
	cache, events := fix7rReviewCache(t, backing)
	cache.mu.Lock()
	cache.markDirtyLocked(bead.ID)
	cache.mu.Unlock()
	events.reset()
	backing.afterGet = func() {
		if err := mem.Close(bead.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "closed", Live: true}); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, bead.ID, 1)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("dirty Get status = %q, want closed", got.Status)
	}
	cache.runReconciliation()
	requireCloseCount(t, events, bead.ID, 1)
}

func TestStaleDirtyOverlayDoesNotReopenReadReportedClose(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "dirty overlay"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &fix7r3StaleGetStore{MemStore: mem, targetID: bead.ID}
	cache, events := fix7rReviewCache(t, backing)
	cache.mu.Lock()
	cache.markDirtyLocked(bead.ID)
	cache.mu.Unlock()
	events.reset()
	backing.afterGet = func() {
		if err := mem.Close(bead.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "closed", Live: true}); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, bead.ID, 1)
	}
	open, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("dirty overlay open rows = %+v, want none", open)
	}
	cache.runReconciliation()
	requireCloseCount(t, events, bead.ID, 1)
}

func TestClosedRowEvictionRetainsOlderReconcileFence(t *testing.T) {
	backing := &afterScanStore{MemStore: NewMemStoreFrom(1, []Bead{{ID: "a", Status: "open", ParentID: "p"}}, nil)}
	cache, events := fix7rReviewCache(t, backing)
	events.reset()
	backing.afterScan = func() {
		if err := backing.Close("a"); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "closed", Live: true}); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, "a", 1)
		if err := backing.Delete("a"); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{ParentID: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	cache.runReconciliation()
	cache.runReconciliation()
	requireCloseCount(t, events, "a", 1)
}

func TestWritePathReportedCloseSurvivesAgedReadAndStaleReconcile(t *testing.T) {
	mem := NewMemStore()
	x, err := mem.Create(Bead{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &afterScanStore{MemStore: mem}
	cache, events := fix7rReviewCache(t, backing)
	events.reset()
	backing.afterScan = func() {
		if err := mem.Close(x.ID); err != nil {
			t.Fatal(err)
		}
		if err := cache.SetMetadata(x.ID, "k", "v"); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, x.ID, 1)
		cache.mu.Lock()
		cache.localBeadAt[x.ID] = time.Now().Add(-time.Minute)
		cache.mu.Unlock()
		if _, err := cache.List(ListQuery{Status: "closed", Live: true}); err != nil {
			t.Fatal(err)
		}
	}
	cache.runReconciliation()
	cache.runReconciliation()
	requireCloseCount(t, events, x.ID, 1)
}

func TestStaleReconcileAfterReadAbsorbReportsCloseOnce(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "stale reconcile"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &afterScanStore{MemStore: mem}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	events.reset()
	backing.afterScan = func() {
		if err := mem.Close(bead.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatal(err)
		}
	}

	cache.runReconciliation()
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

func TestCloseAbsorbSurvivesOlderReconcile(t *testing.T) {
	t.Parallel()

	backing := NewMemStoreFrom(1, []Bead{{ID: "a", Status: "open"}}, nil)
	cache, events := fix7rReviewCache(t, backing)
	old, err := backing.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	seq := cache.mutationSeq
	if err := backing.Close("a"); err != nil {
		t.Fatal(err)
	}
	events.reset()
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	res := cache.mergeSnapshotLocked(map[string]Bead{"a": old}, nil, map[string][]Dep{"a": nil}, true, seq, time.Now())
	cache.mu.Unlock()
	cache.notifyChanges(res.notifications)
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	requireCloseCount(t, events, "a", 1)
}

func TestReadCloseFenceSurvivesSecondLiveRead(t *testing.T) {
	t.Parallel()

	backing := &afterScanStore{MemStore: NewMemStoreFrom(1, []Bead{{ID: "a", Status: "open"}}, nil)}
	cache, events := fix7rReviewCache(t, backing)
	events.reset()
	backing.afterScan = func() {
		if err := backing.Close("a"); err != nil {
			t.Fatal(err)
		}
		for _, status := range []string{"open", "closed"} {
			if _, err := cache.List(ListQuery{Status: status, Live: true}); err != nil {
				t.Fatal(err)
			}
		}
	}

	cache.runReconciliation()
	cache.runReconciliation()
	requireCloseCount(t, events, "a", 1)
}

func TestLiveRefreshSeesReopenAfterReportedClose(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopened"})
	if err != nil {
		t.Fatal(err)
	}
	cache, events := fix7rReviewCache(t, backing)
	if err := backing.Close(bead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "closed", Live: true}); err != nil {
		t.Fatal(err)
	}
	requireCloseCount(t, events, bead.ID, 1)
	if err := backing.Reopen(bead.ID); err != nil {
		t.Fatal(err)
	}
	got, err := cache.List(ListQuery{Status: "open", Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("live reopened rows = %+v, want %s", got, bead.ID)
	}
}

func TestReadReportedCloseDoesNotHideLaterForeignReopen(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "reopened"})
	if err != nil {
		t.Fatal(err)
	}
	cache, events := fix7rReviewCache(t, backing)
	if err := backing.Close(bead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	requireCloseCount(t, events, bead.ID, 1)
	if err := backing.Reopen(bead.ID); err != nil {
		t.Fatal(err)
	}
	got, err := cache.List(ListQuery{Status: "open", Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != bead.ID {
		t.Fatalf("live reopened rows = %+v, want %s", got, bead.ID)
	}
}

func TestLaterReopenReadKeepsOlderLiveReadFenced(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "reopen race"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &fix7r3StaleLiveListStore{MemStore: mem}
	cache, events := fix7rReviewCache(t, backing)
	events.reset()
	var reopened Bead
	backing.afterLiveList = func() {
		if err := mem.Close(bead.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, bead.ID, 1)
		if err := mem.Reopen(bead.ID); err != nil {
			t.Fatal(err)
		}
		reopened, err = mem.Get(bead.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := cache.List(ListQuery{Status: "open", Live: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Revision != reopened.Revision {
			t.Fatalf("post-fence read = %+v, want revision %d", got, reopened.Revision)
		}
	}
	got, err := cache.List(ListQuery{Status: "open", Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Revision != reopened.Revision {
		t.Fatalf("older read restored %+v, want revision %d", got, reopened.Revision)
	}
}

func TestReconcileCannotClearFenceForOlderLiveRead(t *testing.T) {
	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "reconcile race"})
	if err != nil {
		t.Fatal(err)
	}
	backing := &fix7r3StaleLiveListStore{MemStore: mem}
	cache, events := fix7rReviewCache(t, backing)
	events.reset()
	backing.afterLiveList = func() {
		if err := mem.Close(bead.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatal(err)
		}
		requireCloseCount(t, events, bead.ID, 1)
		cache.runReconciliation()
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("status after older read = %q, want closed", got.Status)
	}
	cache.runReconciliation()
	requireCloseCount(t, events, bead.ID, 1)
}

func TestPanickingTxDoesNotSilenceLaterForeignCloses(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "panic tx"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = cache.Tx("panics", func(Tx) error {
			panic("boom")
		})
	}()
	if err := backing.Close(bead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

type panicCloseStore struct {
	Store
}

func (s panicCloseStore) Close(string) error {
	panic("boom")
}

func TestPanickingCloseDoesNotSilenceLaterForeignCloses(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "panic close"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(panicCloseStore{Store: mem}, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = cache.Close(bead.ID)
	}()
	if err := mem.Close(bead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

type panicCloseIfMatchStore struct {
	Store
}

func (s panicCloseIfMatchStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	writer, ok := ConditionalWriterFor(s.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	return writer.UpdateIfMatch(id, expectedRevision, opts)
}

func (s panicCloseIfMatchStore) CloseIfMatch(string, int64) error {
	panic("boom")
}

func (s panicCloseIfMatchStore) DeleteIfMatch(id string, expectedRevision int64) error {
	writer, ok := ConditionalWriterFor(s.Store)
	if !ok {
		return ErrConditionalWriteUnsupported
	}
	return writer.DeleteIfMatch(id, expectedRevision)
}

func (s panicCloseIfMatchStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := ConditionalWriterFor(s.Store)
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	return writer.CompareAndSetMetadataKey(id, key, expected, next)
}

func TestPanickingCloseIfMatchDoesNotSilenceLaterForeignCloses(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	bead, err := mem.Create(Bead{Title: "panic conditional close"})
	if err != nil {
		t.Fatal(err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(panicCloseIfMatchStore{Store: mem}, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = recover() }()
		_ = cache.CloseIfMatch(bead.ID, got.Revision)
	}()
	if err := mem.Close(bead.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatal(err)
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}

func TestLiveRefreshClosedRowReportsCloseOnAbsorb(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "closed behind cache"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	events.reset()
	liveOpen, err := cache.List(ListQuery{Status: "open", Live: true})
	if err != nil {
		t.Fatalf("Live open list: %v", err)
	}
	if len(liveOpen) != 0 {
		t.Fatalf("Live open list = %#v, want closed bead omitted", liveOpen)
	}

	requireCloseCount(t, &events, bead.ID, 1)
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
	assertNotCached(t, cache, bead.ID)
}

func TestDirtyOverlayClosedRowReportsCloseOnAbsorb(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "dirty closed behind cache"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.Lock()
	cache.markDirtyLocked(bead.ID)
	cache.mu.Unlock()
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	events.reset()
	open, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("cached open list with dirty overlay: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("cached open list = %#v, want closed dirty bead suppressed", open)
	}

	requireCloseCount(t, &events, bead.ID, 1)
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
	assertNotCached(t, cache, bead.ID)
}

func TestDirtyGetClosedRowReportsCloseOnAbsorb(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "dirty get closed behind cache"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	cache.mu.Lock()
	cache.markDirtyLocked(bead.ID)
	cache.mu.Unlock()
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	events.reset()
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get dirty closed bead: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("Get dirty closed bead status = %q, want closed", got.Status)
	}

	requireCloseCount(t, &events, bead.ID, 1)
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
	assertNotCached(t, cache, bead.ID)
}

func TestForeignCloseRetainsBlockedOutcome(t *testing.T) {
	t.Parallel()

	backing := NewMemStoreFrom(2, []Bead{
		{ID: "a", Status: "open", Type: "task"},
		{ID: "b", Status: "open", Type: "task"},
	}, []Dep{{IssueID: "b", DependsOnID: "a", Type: "blocks"}})
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.SetMetadata("a", beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkOutcomeBlocked); err != nil {
		t.Fatalf("SetMetadata backing: %v", err)
	}
	if err := backing.Close("a"); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatalf("Live open list: %v", err)
	}
	want, err := backing.Ready()
	if err != nil {
		t.Fatalf("backing Ready: %v", err)
	}
	if len(want) != 0 {
		t.Fatalf("fixture backing Ready = %+v, want none", want)
	}
	got, err := cache.Ready()
	if err != nil {
		t.Fatalf("cache Ready: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cache Ready = %+v, want blocked dependent omitted", got)
	}
}

func TestForeignCloseSurvivesEveryRefreshShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query ListQuery
		setup func(*CachingStore, string)
	}{
		{
			name:  "parent",
			query: ListQuery{ParentID: "p"},
		},
		{
			name:  "history",
			query: ListQuery{Status: "closed", Live: true},
		},
		{
			name:  "aged-local",
			query: ListQuery{Status: "open", Live: true},
			setup: func(cache *CachingStore, id string) {
				cache.mu.Lock()
				cache.noteLocalMutationLocked(id)
				cache.localBeadAt[id] = time.Now().Add(-time.Minute)
				cache.mu.Unlock()
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backing := NewMemStoreFrom(1, []Bead{
				{ID: "a", Status: "open", Type: "task", ParentID: "p"},
			}, nil)
			var events cacheEventRecorder
			cache := NewCachingStoreForTest(backing, events.callback)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if tc.setup != nil {
				tc.setup(cache, "a")
			}
			if err := backing.Close("a"); err != nil {
				t.Fatalf("Close backing: %v", err)
			}

			events.reset()
			if _, err := cache.List(tc.query); err != nil {
				t.Fatalf("List(%+v): %v", tc.query, err)
			}
			requireCloseCount(t, &events, "a", 1)
			cache.runReconciliation()
			cache.runReconciliation()
			requireCloseCount(t, &events, "a", 1)
		})
	}
}

func TestParentListClosedChildReportsClose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		read func(*CachingStore, string) error
	}{
		{
			name: "include-closed",
			read: func(cache *CachingStore, parentID string) error {
				_, err := cache.Children(parentID, IncludeClosed)
				return err
			},
		},
		{
			name: "parent-active",
			read: func(cache *CachingStore, parentID string) error {
				_, err := cache.List(ListQuery{ParentID: parentID})
				return err
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backing := NewMemStore()
			parent, err := backing.Create(Bead{Title: "parent"})
			if err != nil {
				t.Fatalf("Create parent: %v", err)
			}
			child, err := backing.Create(Bead{Title: "child", ParentID: parent.ID})
			if err != nil {
				t.Fatalf("Create child: %v", err)
			}
			var events cacheEventRecorder
			cache := NewCachingStoreForTest(backing, events.callback)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if err := backing.Close(child.ID); err != nil {
				t.Fatalf("Close backing child: %v", err)
			}

			events.reset()
			if err := tc.read(cache, parent.ID); err != nil {
				t.Fatalf("read parent children: %v", err)
			}
			requireCloseCount(t, &events, child.ID, 1)
			cache.runReconciliation()
			requireCloseCount(t, &events, child.ID, 1)
		})
	}
}

func TestForeignCloseMissingRowsReportCloseOnEvict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		read func(*CachingStore, string) error
	}{
		{
			name: "parent",
			read: func(cache *CachingStore, parentID string) error {
				_, err := cache.List(ListQuery{ParentID: parentID})
				return err
			},
		},
		{
			name: "live-missing",
			read: func(cache *CachingStore, _ string) error {
				_, err := cache.List(ListQuery{Status: "open", Live: true})
				return err
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backing := NewMemStore()
			parent, err := backing.Create(Bead{Title: "parent"})
			if err != nil {
				t.Fatalf("Create parent: %v", err)
			}
			child, err := backing.Create(Bead{Title: "child", ParentID: parent.ID})
			if err != nil {
				t.Fatalf("Create child: %v", err)
			}
			var events cacheEventRecorder
			cache := NewCachingStoreForTest(backing, events.callback)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			if err := backing.Delete(child.ID); err != nil {
				t.Fatalf("Delete backing child: %v", err)
			}

			events.reset()
			if err := tc.read(cache, parent.ID); err != nil {
				t.Fatalf("read after backing deletion: %v", err)
			}
			requireCloseCount(t, &events, child.ID, 1)
			if _, err := cache.Get(child.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after eviction error = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestForeignCloseAbsorbedByLocalWriteReportsClose(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T, *MemStore, string) func(*CachingStore, string) error
	}{
		{
			name: "update",
			setup: func(_ *testing.T, _ *MemStore, _ string) func(*CachingStore, string) error {
				return func(cache *CachingStore, id string) error {
					title := "local update before foreign close"
					return cache.Update(id, UpdateOpts{Title: &title})
				}
			},
		},
		{
			name: "release-if-current",
			setup: func(t *testing.T, backing *MemStore, id string) func(*CachingStore, string) error {
				t.Helper()
				status := "in_progress"
				assignee := "worker"
				if err := backing.Update(id, UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
					t.Fatalf("prepare in-progress bead: %v", err)
				}
				return func(cache *CachingStore, id string) error {
					released, err := cache.ReleaseIfCurrent(id, assignee)
					if err != nil {
						return err
					}
					if !released {
						return errors.New("ReleaseIfCurrent returned false")
					}
					return nil
				}
			},
		},
		{
			name: "set-metadata",
			setup: func(_ *testing.T, _ *MemStore, _ string) func(*CachingStore, string) error {
				return func(cache *CachingStore, id string) error {
					return cache.SetMetadata(id, "local", "write")
				}
			},
		},
		{
			name: "set-metadata-batch",
			setup: func(_ *testing.T, _ *MemStore, _ string) func(*CachingStore, string) error {
				return func(cache *CachingStore, id string) error {
					return cache.SetMetadataBatch(id, map[string]string{"local": "batch"})
				}
			},
		},
		{
			name: "dep-add",
			setup: func(t *testing.T, backing *MemStore, _ string) func(*CachingStore, string) error {
				t.Helper()
				blocker, err := backing.Create(Bead{Title: "blocker"})
				if err != nil {
					t.Fatalf("Create blocker: %v", err)
				}
				return func(cache *CachingStore, id string) error {
					return cache.DepAdd(id, blocker.ID, "blocks")
				}
			},
		},
		{
			name: "dep-remove",
			setup: func(t *testing.T, backing *MemStore, id string) func(*CachingStore, string) error {
				t.Helper()
				blocker, err := backing.Create(Bead{Title: "blocker"})
				if err != nil {
					t.Fatalf("Create blocker: %v", err)
				}
				if err := backing.DepAdd(id, blocker.ID, "blocks"); err != nil {
					t.Fatalf("prepare dependency: %v", err)
				}
				return func(cache *CachingStore, id string) error {
					return cache.DepRemove(id, blocker.ID)
				}
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mem := NewMemStore()
			backing := &closeAfterWriteStore{Store: mem}
			bead, err := mem.Create(Bead{Title: "write target"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			write := tc.setup(t, mem, bead.ID)
			var events cacheEventRecorder
			cache := NewCachingStoreForTest(backing, events.callback)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}

			events.reset()
			if err := write(cache, bead.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			requireEventSequence(t, &events, "bead.updated:"+bead.ID, "bead.closed:"+bead.ID)
			cache.runReconciliation()
			requireEventSequence(t, &events, "bead.updated:"+bead.ID, "bead.closed:"+bead.ID)
		})
	}
}

type closeAfterWriteStore struct {
	Store
}

func (s *closeAfterWriteStore) Update(id string, opts UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	return s.Store.Close(id)
}

func (s *closeAfterWriteStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(ConditionalAssignmentReleaser)
	if !ok {
		return false, ErrConditionalReleaseUnsupported
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if err != nil || !released {
		return released, err
	}
	return true, s.Store.Close(id)
}

func (s *closeAfterWriteStore) SetMetadata(id, key, value string) error {
	if err := s.Store.SetMetadata(id, key, value); err != nil {
		return err
	}
	return s.Store.Close(id)
}

func (s *closeAfterWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.Store.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	return s.Store.Close(id)
}

func (s *closeAfterWriteStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.Store.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	return s.Store.Close(issueID)
}

func (s *closeAfterWriteStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.Store.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}
	return s.Store.Close(issueID)
}

func TestForeignCloseAbsorbDoesNotHoldDirtyMarks(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	var ids []string
	for i := 0; i < dirtyOverlayMaxGets+1; i++ {
		bead, err := backing.Create(Bead{Title: "dirty close"})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, bead.ID)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	for _, id := range ids {
		if err := backing.Close(id); err != nil {
			t.Fatalf("Close backing %s: %v", id, err)
		}
	}
	if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
		t.Fatalf("Live open list: %v", err)
	}
	if _, err := cache.Handles().Cached.Ready(); err != nil {
		t.Fatalf("Cached Ready after foreign close absorb = %v, want cache still available", err)
	}
}

func TestForeignCloseAbsorbNotifiesExactlyOnceAcrossConcurrentReadsAndReconcile(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "one event"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var events cacheEventRecorder
	cache := NewCachingStoreForTest(backing, events.callback)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := backing.Close(bead.ID); err != nil {
		t.Fatalf("Close backing: %v", err)
	}

	events.reset()
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := cache.List(ListQuery{Status: "open", Live: true})
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent live read %d: %v", i, err)
		}
	}
	cache.runReconciliation()
	requireCloseCount(t, &events, bead.ID, 1)
}
