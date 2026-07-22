package beads

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type cacheLifecycleMetadataBacking struct {
	*MemStore

	lifecycleEntered chan struct{}
	releaseLifecycle chan struct{}
	ordinaryEntered  chan struct{}
	ordinaryOnce     sync.Once

	mu         sync.Mutex
	inCritical bool
}

func (s *cacheLifecycleMetadataBacking) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) (err error) {
	s.mu.Lock()
	s.inCritical = true
	s.mu.Unlock()
	if s.lifecycleEntered != nil {
		close(s.lifecycleEntered)
	}
	defer func() {
		s.mu.Lock()
		s.inCritical = false
		s.mu.Unlock()
	}()
	if s.releaseLifecycle != nil {
		<-s.releaseLifecycle
	}
	return fn(lifecycleMetadataDirectTransaction{
		id:     id,
		reader: s.MemStore,
		writer: s.MemStore,
	})
}

func (s *cacheLifecycleMetadataBacking) SetMetadata(id, key, value string) error {
	if key == "ordinary" && s.ordinaryEntered != nil {
		s.ordinaryOnce.Do(func() { close(s.ordinaryEntered) })
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *cacheLifecycleMetadataBacking) critical() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inCritical
}

type lifecycleMetadataObservation struct {
	eventType string
	bead      Bead
}

func waitLifecycleMetadataOperation(t *testing.T, name string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not finish", name)
	}
}

func TestCachingStoreLifecycleMetadataSerializesWritesAndPublishesSnapshotsInOrder(t *testing.T) {
	mem := NewMemStore()
	created, err := mem.Create(Bead{Title: "cache lifecycle metadata"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheLifecycleMetadataBacking{
		MemStore:         mem,
		lifecycleEntered: make(chan struct{}),
		releaseLifecycle: make(chan struct{}),
		ordinaryEntered:  make(chan struct{}),
	}

	var observationsMu sync.Mutex
	var observations []lifecycleMetadataObservation
	observerRanInCritical := false
	cache := NewCachingStoreForTest(backing, func(eventType, _ string, payload json.RawMessage) {
		var bead Bead
		if err := json.Unmarshal(payload, &bead); err != nil {
			t.Errorf("unmarshal observer payload: %v", err)
			return
		}
		observationsMu.Lock()
		observations = append(observations, lifecycleMetadataObservation{eventType: eventType, bead: bead})
		observerRanInCritical = observerRanInCritical || backing.critical()
		observationsMu.Unlock()
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	lifecycleDone := make(chan error, 1)
	go func() {
		lifecycleDone <- WithLifecycleMetadataTransaction(cache, created.ID, func(tx LifecycleMetadataTransaction) error {
			if err := tx.SetMetadata("phase", "intent"); err != nil {
				return err
			}
			return tx.SetMetadataBatch(map[string]string{"marker": "committed"})
		})
	}()
	select {
	case <-backing.lifecycleEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cache did not delegate to the backing lifecycle transaction")
	}

	ordinaryDone := make(chan error, 1)
	go func() {
		ordinaryDone <- cache.SetMetadata(created.ID, "ordinary", "later")
	}()
	if mutationReachedBackingBeforeSerialization(t, cache, created.ID, backing.ordinaryEntered, 2) {
		close(backing.releaseLifecycle)
		t.Fatal("ordinary metadata write reached backing while lifecycle callback held the bead lock")
	}

	close(backing.releaseLifecycle)
	waitLifecycleMetadataOperation(t, "lifecycle metadata transaction", lifecycleDone)
	waitLifecycleMetadataOperation(t, "ordinary metadata write", ordinaryDone)

	observationsMu.Lock()
	gotObservations := append([]lifecycleMetadataObservation(nil), observations...)
	ranInCritical := observerRanInCritical
	observationsMu.Unlock()
	if ranInCritical {
		t.Fatal("observer ran before the backing lifecycle critical section released")
	}
	if len(gotObservations) != 3 {
		t.Fatalf("observer notifications = %d, want 3: %#v", len(gotObservations), gotObservations)
	}
	for i, observation := range gotObservations {
		if observation.eventType != "bead.updated" {
			t.Errorf("notification %d event = %q, want bead.updated", i, observation.eventType)
		}
	}
	if got := gotObservations[0].bead.Metadata; got["phase"] != "intent" || got["marker"] != "" || got["ordinary"] != "" {
		t.Errorf("first snapshot metadata = %#v, want only phase mutation", got)
	}
	if got := gotObservations[1].bead.Metadata; got["phase"] != "intent" || got["marker"] != "committed" || got["ordinary"] != "" {
		t.Errorf("second snapshot metadata = %#v, want lifecycle mutations", got)
	}
	if got := gotObservations[2].bead.Metadata; got["phase"] != "intent" || got["marker"] != "committed" || got["ordinary"] != "later" {
		t.Errorf("third snapshot metadata = %#v, want later ordinary mutation", got)
	}

	cached, err := cache.Handles().Cached.Get(created.ID)
	if err != nil {
		t.Fatalf("Cached.Get: %v", err)
	}
	if cached.Metadata["phase"] != "intent" || cached.Metadata["marker"] != "committed" || cached.Metadata["ordinary"] != "later" {
		t.Fatalf("cached metadata = %#v, want all mutations", cached.Metadata)
	}
}

func TestCachingStoreLifecycleMetadataObserversMayReenterAfterLocksRelease(t *testing.T) {
	mem := NewMemStore()
	created, err := mem.Create(Bead{Title: "reentrant lifecycle observer"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheLifecycleMetadataBacking{MemStore: mem}

	var cache *CachingStore
	var reenterOnce sync.Once
	var reenterErr error
	cache = NewCachingStoreForTest(backing, func(_ string, _ string, payload json.RawMessage) {
		var bead Bead
		if err := json.Unmarshal(payload, &bead); err != nil {
			t.Errorf("unmarshal observer payload: %v", err)
			return
		}
		if bead.Metadata["phase"] == "intent" && bead.Metadata["observer"] == "" {
			reenterOnce.Do(func() {
				reenterErr = cache.SetMetadata(created.ID, "observer", "reentered")
			})
		}
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- WithLifecycleMetadataTransaction(cache, created.ID, func(tx LifecycleMetadataTransaction) error {
			return tx.SetMetadata("phase", "intent")
		})
	}()
	waitLifecycleMetadataOperation(t, "reentrant lifecycle metadata transaction", done)
	if reenterErr != nil {
		t.Fatalf("reentrant observer mutation: %v", reenterErr)
	}
	got, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if got.Metadata["observer"] != "reentered" {
		t.Fatalf("observer metadata = %q, want reentered", got.Metadata["observer"])
	}
}

func TestCachingStoreLifecycleMetadataRetainsSuccessfulMutationBeforeCallbackError(t *testing.T) {
	callbackErr := errors.New("later lifecycle callback failure")
	mem := NewMemStore()
	created, err := mem.Create(Bead{Title: "callback failure"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheLifecycleMetadataBacking{MemStore: mem}
	var notifications int
	cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) { notifications++ })
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	err = WithLifecycleMetadataTransaction(cache, created.ID, func(tx LifecycleMetadataTransaction) error {
		if err := tx.SetMetadata("phase", "durable"); err != nil {
			return err
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("transaction error = %v, want unchanged callback error %v", err, callbackErr)
	}
	cached, err := cache.Handles().Cached.Get(created.ID)
	if err != nil {
		t.Fatalf("Cached.Get: %v", err)
	}
	if cached.Metadata["phase"] != "durable" {
		t.Fatalf("cached phase = %q, want durable", cached.Metadata["phase"])
	}
	if notifications != 1 {
		t.Fatalf("observer notifications = %d, want one successful mutation snapshot", notifications)
	}
}

type cacheLifecycleMetadataReadFailureBacking struct {
	*MemStore
	getErr error
}

func (s *cacheLifecycleMetadataReadFailureBacking) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return fn(&cacheLifecycleMetadataReadFailureTransaction{
		id:      id,
		store:   s.MemStore,
		getErr:  s.getErr,
		failGet: 2,
	})
}

type cacheLifecycleMetadataReadFailureTransaction struct {
	id        string
	store     *MemStore
	getErr    error
	mutations int
	failGet   int
}

func (tx *cacheLifecycleMetadataReadFailureTransaction) Get() (Bead, error) {
	if tx.mutations == tx.failGet {
		return Bead{}, tx.getErr
	}
	return tx.store.Get(tx.id)
}

func (tx *cacheLifecycleMetadataReadFailureTransaction) SetMetadata(key, value string) error {
	if err := tx.store.SetMetadata(tx.id, key, value); err != nil {
		return err
	}
	tx.mutations++
	return nil
}

func (tx *cacheLifecycleMetadataReadFailureTransaction) SetMetadataBatch(values map[string]string) error {
	if err := tx.store.SetMetadataBatch(tx.id, values); err != nil {
		return err
	}
	if len(values) > 0 {
		tx.mutations++
	}
	return nil
}

func TestCachingStoreLifecycleMetadataReadFailureKeepsEarlierSnapshotsAndDirtiesCache(t *testing.T) {
	snapshotErr := errors.New("post-mutation snapshot unavailable")
	mem := NewMemStore()
	blocker, err := mem.Create(Bead{Title: "snapshot blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	created, err := mem.Create(Bead{Title: "snapshot failure", Needs: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheLifecycleMetadataReadFailureBacking{MemStore: mem, getErr: snapshotErr}
	var notifications []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notifications = append(notifications, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	err = WithLifecycleMetadataTransaction(cache, created.ID, func(tx LifecycleMetadataTransaction) error {
		if err := tx.SetMetadata("first", "retained"); err != nil {
			return err
		}
		_ = tx.SetMetadata("second", "durable-but-unsnapshotted")
		return nil
	})
	if !errors.Is(err, snapshotErr) {
		t.Fatalf("transaction error = %v, want %v", err, snapshotErr)
	}

	durable, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if durable.Metadata["first"] != "retained" || durable.Metadata["second"] != "durable-but-unsnapshotted" {
		t.Fatalf("durable metadata = %#v, want both successful mutations", durable.Metadata)
	}
	cache.mu.RLock()
	cached := cloneBead(cache.beads[created.ID])
	_, dirty := cache.dirty[created.ID]
	cache.mu.RUnlock()
	if cached.Metadata["first"] != "retained" || cached.Metadata["second"] != "" {
		t.Fatalf("cached snapshot metadata = %#v, want only first retained snapshot", cached.Metadata)
	}
	if !dirty {
		t.Fatal("cache was not marked dirty after a successful mutation could not be snapshotted")
	}
	if len(notifications) != 1 {
		t.Fatalf("observer notifications = %d, want one retained snapshot", len(notifications))
	}
	if _, err := cache.Get(created.ID); err != nil {
		t.Fatalf("authoritative Get after failed snapshot: %v", err)
	}
	expireCacheMutationRecencyForTest(cache, created.ID)
	cache.runReconciliation()
	cache.runReconciliation()
	if len(notifications) != 2 || notifications[1].eventType != "bead.updated" {
		t.Fatalf("observer notifications = %+v, want retained snapshot then recovered update", notifications)
	}
	recovered, ok := DecodeBeadEventPayload(notifications[1].payload)
	if !ok || recovered.Metadata["first"] != "retained" ||
		recovered.Metadata["second"] != "durable-but-unsnapshotted" ||
		len(recovered.Dependencies) != 1 || recovered.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("recovered payload = %+v ok=%v, want complete durable lifecycle snapshot", recovered, ok)
	}
}

type cacheLifecycleMetadataPartialBatchBacking struct {
	*MemStore
	batchErr error
}

func (s *cacheLifecycleMetadataPartialBatchBacking) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	return fn(cacheLifecycleMetadataPartialBatchTransaction{
		id:       id,
		store:    s.MemStore,
		batchErr: s.batchErr,
	})
}

type cacheLifecycleMetadataPartialBatchTransaction struct {
	id       string
	store    *MemStore
	batchErr error
}

func (tx cacheLifecycleMetadataPartialBatchTransaction) Get() (Bead, error) {
	return tx.store.Get(tx.id)
}

func (tx cacheLifecycleMetadataPartialBatchTransaction) SetMetadata(key, value string) error {
	return tx.store.SetMetadata(tx.id, key, value)
}

func (tx cacheLifecycleMetadataPartialBatchTransaction) SetMetadataBatch(values map[string]string) error {
	if value, ok := values["partial"]; ok {
		if err := tx.store.SetMetadata(tx.id, "partial", value); err != nil {
			return err
		}
	}
	return tx.batchErr
}

func TestCachingStoreLifecycleMetadataPartialBatchErrorDirtiesCacheWithoutFabricatingSnapshot(t *testing.T) {
	batchErr := errors.New("metadata batch partially applied")
	mem := NewMemStore()
	created, err := mem.Create(Bead{Title: "partial lifecycle batch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheLifecycleMetadataPartialBatchBacking{MemStore: mem, batchErr: batchErr}
	var notifications int
	cache := NewCachingStoreForTest(backing, func(string, string, json.RawMessage) { notifications++ })
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	err = WithLifecycleMetadataTransaction(cache, created.ID, func(tx LifecycleMetadataTransaction) error {
		return tx.SetMetadataBatch(map[string]string{
			"partial": "landed",
			"missing": "unknown",
		})
	})
	if !errors.Is(err, batchErr) {
		t.Fatalf("transaction error = %v, want %v", err, batchErr)
	}
	durable, err := mem.Get(created.ID)
	if err != nil {
		t.Fatalf("backing Get: %v", err)
	}
	if durable.Metadata["partial"] != "landed" {
		t.Fatalf("durable partial metadata = %q, want landed", durable.Metadata["partial"])
	}
	cache.mu.RLock()
	cached := cloneBead(cache.beads[created.ID])
	_, dirty := cache.dirty[created.ID]
	cache.mu.RUnlock()
	if cached.Metadata["partial"] != "" {
		t.Fatalf("cached metadata fabricated partial write: %#v", cached.Metadata)
	}
	if !dirty {
		t.Fatal("cache was not marked dirty after a partially applied batch")
	}
	if notifications != 0 {
		t.Fatalf("observer notifications = %d, want none without an authoritative snapshot", notifications)
	}
}
