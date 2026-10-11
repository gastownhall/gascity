package beads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCachingStoreObservedListResultContract(t *testing.T) {
	backing := &observationReadCountingStore{Store: NewMemStore()}
	if _, err := backing.Create(Bead{Title: "first"}); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	priority := 2
	deferUntil := time.Now().Add(time.Hour).Round(0)
	blocked := true
	second, err := backing.Create(Bead{
		Title:        "second",
		Priority:     &priority,
		Labels:       []string{"label"},
		Needs:        []string{"blocks:dependency"},
		Metadata:     StringMap{"key": "value"},
		Dependencies: []Dep{{DependsOnID: "dependency", Type: "blocks"}},
		DeferUntil:   &deferUntil,
		IsBlocked:    &blocked,
	})
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	beforeReads := backing.readCalls.Load()
	rows, observation, ok := cache.ObservedList(ListQuery{
		Status: "open",
		Sort:   SortCreatedDesc,
		Limit:  1,
	})
	if !ok || len(rows) != 1 || rows[0].ID != second.ID {
		t.Fatalf("ObservedList = %#v, %v; want newest one-row prefix %s", rows, ok, second.ID)
	}

	// Ordinary cache-only reads between observation and conditional use do not
	// invalidate the stamp or consult the backing store.
	if cached, ok := cache.CachedList(ListQuery{Status: "open"}); !ok || len(cached) != 2 {
		t.Fatalf("CachedList between observation and use = %#v, %v; want two cached rows", cached, ok)
	}

	*rows[0].Priority = 99
	rows[0].Labels[0] = "changed"
	rows[0].Needs[0] = "changed"
	rows[0].Metadata["key"] = "changed"
	rows[0].Dependencies[0].DependsOnID = "changed"
	*rows[0].DeferUntil = rows[0].DeferUntil.Add(time.Hour)
	*rows[0].IsBlocked = false

	got, err := cache.Get(second.ID)
	if err != nil {
		t.Fatalf("Get after returned-row mutation: %v", err)
	}
	if !reflect.DeepEqual(got, second) {
		t.Fatalf("cached row changed through ObservedList result:\n got: %#v\nwant: %#v", got, second)
	}

	called := false
	accepted, err := cache.WithCurrentObservation(observation, func() error {
		called = true
		return nil
	})
	if err != nil || !accepted || !called {
		t.Fatalf("WithCurrentObservation = %v, %v, called=%v; want accepted callback", accepted, err, called)
	}
	if got := backing.readCalls.Load(); got != beforeReads {
		t.Fatalf("backing reads = %d after ObservedList/cache-only use, want %d", got, beforeReads)
	}
}

func TestCachingStoreWithCurrentObservationRejectsStaleZeroForeignAndNilCallback(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "work"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	observation := mustObserveActiveCache(t, cache)

	title := "changed"
	if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	for _, tc := range []struct {
		name        string
		observation CacheObservation
	}{
		{name: "zero"},
		{name: "stale", observation: observation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertObservationRejected(t, cache, tc.observation)
		})
	}

	other := NewCachingStoreForTest(NewMemStore(), nil)
	if err := other.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(other): %v", err)
	}
	foreign := mustObserveActiveCache(t, other)
	assertObservationRejected(t, cache, foreign)

	if accepted, err := cache.WithCurrentObservation(mustObserveActiveCache(t, cache), nil); err == nil || accepted {
		t.Fatalf("WithCurrentObservation(nil callback) = %v, %v; want false, error", accepted, err)
	}
}

func TestCachingStoreObservedListRejectsUnavailableQueriesWithoutBackingRead(t *testing.T) {
	backing := &observationReadCountingStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	for _, tc := range []struct {
		name       string
		state      cacheState
		revision   uint64
		query      ListQuery
		dirty      bool
		partialErr error
	}{
		{name: "uninitialized", state: cacheUninitialized, revision: 1, query: ListQuery{Status: "open"}},
		{name: "degraded", state: cacheDegraded, revision: 1, query: ListQuery{Status: "open"}},
		{name: "dirty", state: cacheLive, revision: 1, query: ListQuery{Status: "open"}, dirty: true},
		{name: "partial result", state: cachePartial, revision: 1, query: ListQuery{Status: "open"}, partialErr: errors.New("partial")},
		{name: "zero revision", state: cacheLive, query: ListQuery{Status: "open"}},
		{name: "live", state: cacheLive, revision: 1, query: ListQuery{Status: "open", Live: true}},
		{name: "closed", state: cacheLive, revision: 1, query: ListQuery{Status: "closed"}},
		{name: "include closed", state: cacheLive, revision: 1, query: ListQuery{Status: "open", IncludeClosed: true}},
		{name: "parent", state: cacheLive, revision: 1, query: ListQuery{ParentID: "parent"}},
		{name: "parents", state: cacheLive, revision: 1, query: ListQuery{ParentIDs: []string{"parent"}, AllowScan: true}},
		{name: "invalid", state: cacheLive, revision: 1, query: ListQuery{Assignee: "a", Assignees: []string{"b"}}},
		{name: "scan required", state: cacheLive, revision: 1, query: ListQuery{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache.mu.Lock()
			cache.state = tc.state
			cache.dirty = map[string]struct{}{}
			if tc.dirty {
				cache.dirty["dirty"] = struct{}{}
			}
			cache.primePartialErr = tc.partialErr
			cache.observationRevision = tc.revision
			cache.mu.Unlock()

			beforeReads := backing.readCalls.Load()
			if rows, _, ok := cache.ObservedList(tc.query); ok || rows != nil {
				t.Fatalf("ObservedList = %#v, ok=true; want unavailable", rows)
			}
			if got := backing.readCalls.Load(); got != beforeReads {
				t.Fatalf("backing reads = %d, want %d", got, beforeReads)
			}
		})
	}
}

func TestCachingStoreObservedListRejectsRealPartialPrimeWithoutBackingRead(t *testing.T) {
	mem := NewMemStore()
	if _, err := mem.Create(Bead{Title: "partial survivor"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	partial := &partialListErrorStore{
		Store:           mem,
		partialStatuses: map[string]bool{"open": true},
	}
	backing := &observationReadCountingStore{Store: partial}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	beforeReads := backing.readCalls.Load()
	if rows, _, ok := cache.ObservedList(ListQuery{Status: "open"}); ok || rows != nil {
		t.Fatalf("ObservedList after partial PrimeActive = %#v, true; want unavailable", rows)
	}
	if got := backing.readCalls.Load(); got != beforeReads {
		t.Fatalf("backing reads = %d, want %d", got, beforeReads)
	}
}

func TestCachingStoreObservedListAcceptsCleanPrimeActiveCache(t *testing.T) {
	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "active"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	rows, observation, ok := cache.ObservedList(ListQuery{Status: "open"})
	if !ok || len(rows) != 1 || rows[0].ID != bead.ID {
		t.Fatalf("ObservedList(clean partial) = %#v, %v", rows, ok)
	}
	if accepted, err := cache.WithCurrentObservation(observation, func() error { return nil }); err != nil || !accepted {
		t.Fatalf("WithCurrentObservation(clean partial) = %v, %v", accepted, err)
	}
}

func TestCachingStoreObservationInvalidatedByMutationPaths(t *testing.T) {
	t.Run("local write", func(t *testing.T) {
		_, cache, bead, observation := newObservationFixture(t, Bead{Title: "work"})
		title := "changed"
		if err := cache.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		assertObservationRejected(t, cache, observation)
	})

	t.Run("applied event", func(t *testing.T) {
		backing, cache, _, observation := newObservationFixture(t, Bead{Title: "existing"})
		added, err := backing.Create(Bead{Title: "event-created"})
		if err != nil {
			t.Fatalf("out-of-band Create: %v", err)
		}
		payload, err := json.Marshal(added)
		if err != nil {
			t.Fatalf("Marshal event: %v", err)
		}
		cache.ApplyEvent("bead.created", payload)
		if got, err := cache.Get(added.ID); err != nil || got.Title != added.Title {
			t.Fatalf("Get(event-created) = %#v, %v", got, err)
		}
		assertObservationRejected(t, cache, observation)
	})

	t.Run("live list refresh", func(t *testing.T) {
		backing, cache, bead, observation := newObservationFixture(t, Bead{Title: "before"})
		title := "after"
		if err := backing.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("out-of-band Update: %v", err)
		}
		if _, err := cache.List(ListQuery{Status: "open", Live: true}); err != nil {
			t.Fatalf("List(Live): %v", err)
		}
		if got, err := cache.Get(bead.ID); err != nil || got.Title != title {
			t.Fatalf("Get after Live refresh = %#v, %v", got, err)
		}
		assertObservationRejected(t, cache, observation)
	})

	t.Run("conditional eviction", func(t *testing.T) {
		_, cache, bead, observation := newObservationFixture(t, Bead{Title: "work"})
		cache.evictForConditionalWrite(bead.ID)
		cache.mu.RLock()
		_, cached := cache.beads[bead.ID]
		_, dirty := cache.dirty[bead.ID]
		cache.mu.RUnlock()
		if cached || !dirty {
			t.Fatalf("conditional eviction left cached=%v dirty=%v; want false, true", cached, dirty)
		}
		assertObservationRejected(t, cache, observation)
	})

	t.Run("conditional ambiguous failure", func(t *testing.T) {
		_, cache, bead, observation := newObservationFixture(t, Bead{Title: "work"})
		cache.applyConditionalWriteFailure(bead.ID, errors.New("ambiguous write result"))
		cache.mu.RLock()
		_, dirty := cache.dirty[bead.ID]
		cache.mu.RUnlock()
		if !dirty {
			t.Fatal("ambiguous conditional failure did not mark cached row dirty")
		}
		assertObservationRejected(t, cache, observation)
	})

	t.Run("ready projection", func(t *testing.T) {
		blocked := true
		_, cache, bead, observation := newObservationFixture(t, Bead{
			Title:     "projected",
			IsBlocked: &blocked,
		})
		cache.mu.Lock()
		changed := cache.clearAllReadyProjectionsLocked()
		projected := cache.beads[bead.ID].IsBlocked
		cache.mu.Unlock()
		if !changed || projected != nil {
			t.Fatalf("clearAllReadyProjectionsLocked = %v, projection=%v; want true, nil", changed, projected)
		}
		assertObservationRejected(t, cache, observation)
	})
}

func TestCachingStoreDirtyOverlayRefreshAdvancesObservationWithoutMutationSequence(t *testing.T) {
	backing, cache, bead, observation := newObservationFixture(t, Bead{Title: "before"})
	title := "after"
	if err := backing.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("out-of-band Update: %v", err)
	}

	cache.mu.Lock()
	mutationSeq := cache.mutationSeq
	cache.markDirtyLocked(bead.ID)
	dirtyRevision := cache.observationRevision
	cache.mu.Unlock()

	rows, err := cache.List(ListQuery{Status: "open"})
	if err != nil {
		t.Fatalf("List with dirty overlay: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != title {
		t.Fatalf("List with dirty overlay = %#v, want refreshed title %q", rows, title)
	}
	cache.mu.RLock()
	_, dirty := cache.dirty[bead.ID]
	gotMutationSeq := cache.mutationSeq
	gotRevision := cache.observationRevision
	cache.mu.RUnlock()
	if dirty {
		t.Fatal("dirty overlay left refreshed row dirty")
	}
	if gotRevision == dirtyRevision {
		t.Fatal("dirty overlay publication did not advance observation revision")
	}
	if gotMutationSeq != mutationSeq {
		t.Fatalf("mutationSeq = %d after read refresh, want %d", gotMutationSeq, mutationSeq)
	}
	assertObservationRejected(t, cache, observation)
}

func TestCachingStorePrimePublicationsInvalidateObservationsWithoutMutationSequence(t *testing.T) {
	for _, method := range []string{"PrimeActive", "Prime"} {
		for _, populated := range []bool{false, true} {
			name := method + "/empty"
			if populated {
				name = method + "/populated"
			}
			t.Run(name, func(t *testing.T) {
				backing := NewMemStore()
				if populated {
					if _, err := backing.Create(Bead{Title: "work"}); err != nil {
						t.Fatalf("Create: %v", err)
					}
				}
				cache := NewCachingStoreForTest(backing, nil)
				if err := cache.Prime(context.Background()); err != nil {
					t.Fatalf("initial Prime: %v", err)
				}
				observation := mustObserveActiveCache(t, cache)
				mutationSeq := cacheMutationSeq(cache)

				var err error
				if method == "PrimeActive" {
					err = cache.PrimeActive()
				} else {
					err = cache.Prime(context.Background())
				}
				if err != nil {
					t.Fatalf("%s: %v", method, err)
				}
				if got := cacheMutationSeq(cache); got != mutationSeq {
					t.Fatalf("mutationSeq = %d after %s publication, want %d", got, method, mutationSeq)
				}
				assertObservationRejected(t, cache, observation)
			})
		}
	}
}

func TestCachingStoreReconciliationPublicationsInvalidateObservationsWithoutMutationSequence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		populated bool
		mutate    func(*testing.T, *MemStore, Bead)
	}{
		{name: "unchanged empty"},
		{
			name: "add",
			mutate: func(t *testing.T, backing *MemStore, _ Bead) {
				t.Helper()
				if _, err := backing.Create(Bead{Title: "added"}); err != nil {
					t.Fatalf("out-of-band Create: %v", err)
				}
			},
		},
		{
			name:      "update",
			populated: true,
			mutate: func(t *testing.T, backing *MemStore, bead Bead) {
				t.Helper()
				title := "updated"
				if err := backing.Update(bead.ID, UpdateOpts{Title: &title}); err != nil {
					t.Fatalf("out-of-band Update: %v", err)
				}
			},
		},
		{
			name:      "delete",
			populated: true,
			mutate: func(t *testing.T, backing *MemStore, bead Bead) {
				t.Helper()
				if err := backing.Delete(bead.ID); err != nil {
					t.Fatalf("out-of-band Delete: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := NewMemStore()
			var bead Bead
			if tc.populated {
				var err error
				bead, err = backing.Create(Bead{Title: "work"})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
			}
			cache := NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("Prime: %v", err)
			}
			observation := mustObserveActiveCache(t, cache)
			mutationSeq := cacheMutationSeq(cache)
			if tc.mutate != nil {
				tc.mutate(t, backing, bead)
			}

			cache.runReconciliation()

			if got := cacheMutationSeq(cache); got != mutationSeq {
				t.Fatalf("mutationSeq = %d after reconciliation publication, want %d", got, mutationSeq)
			}
			assertObservationRejected(t, cache, observation)
		})
	}
}

func TestCachingStoreObservationDoesNotResurrectAfterDegradedEmptyRecovery(t *testing.T) {
	backing := &partialListErrorStore{Store: NewMemStore()}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	observation := mustObserveActiveCache(t, cache)
	mutationSeq := cacheMutationSeq(cache)

	backing.partialAllowScan = true
	cache.runReconciliation()
	cache.mu.RLock()
	degraded := cache.state == cacheDegraded
	cache.mu.RUnlock()
	if !degraded {
		t.Fatal("partial reconciliation did not degrade cache")
	}

	backing.partialAllowScan = false
	cache.runReconciliation()
	cache.mu.RLock()
	live := cache.state == cacheLive
	cache.mu.RUnlock()
	if !live {
		t.Fatal("clean empty reconciliation did not restore live cache")
	}
	if got := cacheMutationSeq(cache); got != mutationSeq {
		t.Fatalf("mutationSeq = %d after availability publications, want %d", got, mutationSeq)
	}
	assertObservationRejected(t, cache, observation)
}

func TestCachingStoreWithCurrentObservationBlocksWriterUntilCallbackReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := NewMemStore()
		if _, err := backing.Create(Bead{Title: "work"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(t.Context()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		observation := mustObserveActiveCache(t, cache)
		entered := make(chan struct{})
		release := make(chan struct{})
		callbackResult := make(chan struct {
			accepted bool
			err      error
		}, 1)
		callbackErr := errors.New("callback failure")
		go func() {
			accepted, err := cache.WithCurrentObservation(observation, func() error {
				close(entered)
				<-release
				return callbackErr
			})
			callbackResult <- struct {
				accepted bool
				err      error
			}{accepted: accepted, err: err}
		}()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("callback did not enter")
		}
		if cache.mu.TryLock() {
			cache.mu.Unlock()
			t.Fatal("writer acquired cache lock while observation callback held read lock")
		}
		close(release)
		synctest.Wait()
		result := <-callbackResult
		if !result.accepted || !errors.Is(result.err, callbackErr) {
			t.Fatalf("WithCurrentObservation callback result = accepted:%v err:%v", result.accepted, result.err)
		}
		if !cache.mu.TryLock() {
			t.Fatal("writer could not acquire cache lock after callback returned")
		}
		cache.mu.Unlock()
	})
}

func TestCachingStoreWithCurrentObservationReleasesLockAfterPanic(t *testing.T) {
	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	observation := mustObserveActiveCache(t, cache)
	panicValue := &struct{}{}
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		_, _ = cache.WithCurrentObservation(observation, func() error {
			panic(panicValue)
		})
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %v, want original %p", recovered, panicValue)
	}
	if !cache.mu.TryLock() {
		t.Fatal("cache writer lock remained held after observation callback panic")
	}
	cache.mu.Unlock()
}

func newObservationFixture(t *testing.T, seed Bead) (*MemStore, *CachingStore, Bead, CacheObservation) {
	t.Helper()
	backing := NewMemStore()
	bead, err := backing.Create(seed)
	if err != nil {
		t.Fatalf("Create fixture: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime fixture: %v", err)
	}
	return backing, cache, bead, mustObserveActiveCache(t, cache)
}

func mustObserveActiveCache(t *testing.T, cache *CachingStore) CacheObservation {
	t.Helper()
	_, observation, ok := cache.ObservedList(ListQuery{AllowScan: true})
	if !ok {
		t.Fatal("ObservedList(active scan) unavailable")
	}
	return observation
}

func assertObservationRejected(t *testing.T, cache *CachingStore, observation CacheObservation) {
	t.Helper()
	called := false
	accepted, err := cache.WithCurrentObservation(observation, func() error {
		called = true
		return nil
	})
	if err != nil || accepted || called {
		t.Fatalf("WithCurrentObservation = %v, %v, called=%v; want rejected without callback", accepted, err, called)
	}
}

func cacheMutationSeq(cache *CachingStore) uint64 {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.mutationSeq
}

type observationReadCountingStore struct {
	Store
	readCalls atomic.Int64
}

func (s *observationReadCountingStore) List(query ListQuery) ([]Bead, error) {
	s.readCalls.Add(1)
	return s.Store.List(query)
}

func (s *observationReadCountingStore) Get(id string) (Bead, error) {
	s.readCalls.Add(1)
	return s.Store.Get(id)
}

func (s *observationReadCountingStore) DepList(id, direction string) ([]Dep, error) {
	s.readCalls.Add(1)
	return s.Store.DepList(id, direction)
}

func (s *observationReadCountingStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	s.readCalls.Add(1)
	return s.Store.Ready(query...)
}
