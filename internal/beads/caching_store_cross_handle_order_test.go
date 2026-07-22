package beads

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// crossHandleLifecycleBacking models the production lifecycle lease without
// coupling the regression to a concrete bd or native store. Close releases the
// backing lease before the decorating cache finalizes its snapshot; Reopen
// takes the same lease before committing its mutation.
type crossHandleLifecycleBacking struct {
	*MemStore

	scope              string
	lifecycleMu        *sync.Mutex
	closeLeaseReleased chan struct{}
	reopenEntered      chan struct{}
	reconcileListRead  chan struct{}
	reconcileListWait  chan struct{}
	durableOrder       *[]string
}

func (s *crossHandleLifecycleBacking) CacheMutationScope() string {
	return s.scope
}

func (s *crossHandleLifecycleBacking) CloseWithReasonIfOpen(id, reason string) (CloseTransition, error) {
	s.lifecycleMu.Lock()
	transition, err := s.MemStore.CloseWithReasonIfOpen(id, reason)
	if err == nil && s.durableOrder != nil {
		*s.durableOrder = append(*s.durableOrder, "bead.closed")
	}
	s.lifecycleMu.Unlock()
	if s.closeLeaseReleased != nil {
		close(s.closeLeaseReleased)
	}
	return transition, err
}

func (s *crossHandleLifecycleBacking) Reopen(id string) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.reopenEntered != nil {
		close(s.reopenEntered)
	}
	err := s.MemStore.Reopen(id)
	if err == nil && s.durableOrder != nil {
		*s.durableOrder = append(*s.durableOrder, "bead.updated(open)")
	}
	return err
}

func (s *crossHandleLifecycleBacking) List(query ListQuery) ([]Bead, error) {
	items, err := s.MemStore.List(query)
	if s.reconcileListRead != nil {
		read := s.reconcileListRead
		wait := s.reconcileListWait
		s.reconcileListRead = nil
		s.reconcileListWait = nil
		close(read)
		if wait != nil {
			<-wait
		}
	}
	return items, err
}

func TestCachingStoreOrdersLifecycleObserversAcrossReplacementHandles(t *testing.T) {
	scope := t.TempDir()
	assertCachingStoreOrdersLifecycleObserversAcrossReplacementHandles(t, scope, scope)
}

func TestCachingStoreOrdersLifecycleObserversAcrossReplacementHandleSymlinkAlias(t *testing.T) {
	root := t.TempDir()
	physicalScope := filepath.Join(root, "backing")
	if err := os.Mkdir(physicalScope, 0o755); err != nil {
		t.Fatalf("Mkdir(physical scope): %v", err)
	}
	aliasScope := filepath.Join(root, "replacement-alias")
	if err := os.Symlink(physicalScope, aliasScope); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	assertCachingStoreOrdersLifecycleObserversAcrossReplacementHandles(t, physicalScope, aliasScope)
}

func TestCachingStoreReplacementReconcileDoesNotDuplicateConcurrentClose(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "replacement reconcile close order"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	scope := t.TempDir()
	lifecycleMu := &sync.Mutex{}
	oldBacking := &crossHandleLifecycleBacking{
		MemStore:           base,
		scope:              scope,
		lifecycleMu:        lifecycleMu,
		closeLeaseReleased: make(chan struct{}),
	}
	replacementBacking := &crossHandleLifecycleBacking{
		MemStore:    base,
		scope:       scope,
		lifecycleMu: lifecycleMu,
	}

	var (
		notesMu sync.Mutex
		notes   []cacheWriteNotification
	)
	observer := func(eventType, beadID string, payload json.RawMessage) {
		notesMu.Lock()
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		notesMu.Unlock()
	}
	oldHandle := NewCachingStoreForTest(oldBacking, observer)
	replacement := NewCachingStoreForTest(replacementBacking, observer)
	if err := oldHandle.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(old): %v", err)
	}
	if err := replacement.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(replacement): %v", err)
	}

	// Hold the old cache immediately after its durable close. The replacement
	// scan observes the now-absent row while it still has a cached open copy,
	// then waits behind the old handle's shared mutation scope. Without a shared
	// snapshot-generation fence it synthesizes a second bead.closed after the
	// old handle publishes the authoritative transition.
	oldHandle.mu.Lock()
	closeDone := make(chan error, 1)
	go func() {
		_, err := oldHandle.closeWithReasonIfOpen(created.ID, "resolved", true)
		closeDone <- err
	}()
	select {
	case <-oldBacking.closeLeaseReleased:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("old handle did not commit close")
	}

	listRead := make(chan struct{})
	replacementBacking.reconcileListRead = listRead
	reconcileDone := make(chan struct{})
	go func() {
		replacement.runReconciliation()
		close(reconcileDone)
	}()
	select {
	case <-listRead:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement reconciliation did not read its snapshot")
	}

	oldHandle.mu.Unlock()
	awaitMutationError(t, "old-handle close", closeDone)
	select {
	case <-reconcileDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement reconciliation did not complete")
	}

	notesMu.Lock()
	gotNotes := slices.Clone(notes)
	notesMu.Unlock()
	if len(gotNotes) != 1 || gotNotes[0].eventType != "bead.closed" || gotNotes[0].beadID != created.ID {
		t.Fatalf("observer notifications = %+v, want one authoritative bead.closed", gotNotes)
	}
	closed, ok := DecodeBeadEventPayload(gotNotes[0].payload)
	if !ok || closed.ID != created.ID || closed.Status != "closed" {
		t.Fatalf("bead.closed payload = %+v ok=%v, want authoritative closed snapshot", closed, ok)
	}
}

func TestCachingStoreReplacementReconcileDoesNotPublishStaleOpenAfterClose(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "replacement stale snapshot order"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	scope := t.TempDir()
	lifecycleMu := &sync.Mutex{}
	oldBacking := &crossHandleLifecycleBacking{MemStore: base, scope: scope, lifecycleMu: lifecycleMu}
	replacementBacking := &crossHandleLifecycleBacking{MemStore: base, scope: scope, lifecycleMu: lifecycleMu}

	var (
		notesMu sync.Mutex
		notes   []cacheWriteNotification
	)
	observer := func(eventType, beadID string, payload json.RawMessage) {
		notesMu.Lock()
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		notesMu.Unlock()
	}
	oldHandle := NewCachingStoreForTest(oldBacking, observer)
	replacement := NewCachingStoreForTest(replacementBacking, observer)
	if err := oldHandle.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(old): %v", err)
	}
	if err := replacement.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(replacement): %v", err)
	}
	if err := base.SetMetadata(created.ID, "phase", "stale-open-snapshot"); err != nil {
		t.Fatalf("external SetMetadata: %v", err)
	}

	listRead := make(chan struct{})
	releaseList := make(chan struct{})
	replacementBacking.reconcileListRead = listRead
	replacementBacking.reconcileListWait = releaseList
	reconcileDone := make(chan struct{})
	go func() {
		replacement.runReconciliation()
		close(reconcileDone)
	}()
	select {
	case <-listRead:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement reconciliation did not capture its open snapshot")
	}

	if _, err := oldHandle.CloseWithReasonIfOpen(created.ID, "resolved"); err != nil {
		t.Fatalf("old-handle CloseWithReasonIfOpen: %v", err)
	}
	close(releaseList)
	select {
	case <-reconcileDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement reconciliation did not complete")
	}

	notesMu.Lock()
	gotNotes := slices.Clone(notes)
	notesMu.Unlock()
	if len(gotNotes) != 1 || gotNotes[0].eventType != "bead.closed" || gotNotes[0].beadID != created.ID {
		t.Fatalf("observer notifications = %+v, want close with no later stale-open update", gotNotes)
	}
	closed, ok := DecodeBeadEventPayload(gotNotes[0].payload)
	if !ok || closed.ID != created.ID || closed.Status != "closed" {
		t.Fatalf("bead.closed payload = %+v ok=%v, want authoritative closed snapshot", closed, ok)
	}
}

func TestCachingStoreReplacementReconcileFencesOnlyConcurrentlyMutatedBead(t *testing.T) {
	base := NewMemStore()
	mutated, err := base.Create(Bead{Title: "mutated by old handle"})
	if err != nil {
		t.Fatalf("Create(mutated): %v", err)
	}
	refreshed, err := base.Create(Bead{Title: "before external refresh"})
	if err != nil {
		t.Fatalf("Create(refreshed): %v", err)
	}

	scope := t.TempDir()
	lifecycleMu := &sync.Mutex{}
	oldBacking := &crossHandleLifecycleBacking{MemStore: base, scope: scope, lifecycleMu: lifecycleMu}
	replacementBacking := &crossHandleLifecycleBacking{MemStore: base, scope: scope, lifecycleMu: lifecycleMu}
	oldHandle := NewCachingStoreForTest(oldBacking, nil)
	replacement := NewCachingStoreForTest(replacementBacking, nil)
	if err := oldHandle.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(old): %v", err)
	}
	if err := replacement.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(replacement): %v", err)
	}

	refreshedTitle := "fresh unrelated snapshot"
	if err := base.Update(refreshed.ID, UpdateOpts{Title: &refreshedTitle}); err != nil {
		t.Fatalf("external Update(refreshed): %v", err)
	}

	listRead := make(chan struct{})
	releaseList := make(chan struct{})
	replacementBacking.reconcileListRead = listRead
	replacementBacking.reconcileListWait = releaseList
	reconcileDone := make(chan struct{})
	go func() {
		replacement.runReconciliation()
		close(reconcileDone)
	}()
	select {
	case <-listRead:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement reconciliation did not capture its snapshot")
	}

	if _, err := oldHandle.CloseWithReasonIfOpen(mutated.ID, "resolved concurrently"); err != nil {
		t.Fatalf("old-handle CloseWithReasonIfOpen: %v", err)
	}
	close(releaseList)
	select {
	case <-reconcileDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("replacement reconciliation did not complete")
	}

	got, err := replacement.Get(refreshed.ID)
	if err != nil {
		t.Fatalf("replacement Get(refreshed): %v", err)
	}
	if got.Title != refreshedTitle {
		t.Fatalf("unrelated refreshed title = %q, want %q", got.Title, refreshedTitle)
	}
}

func assertCachingStoreOrdersLifecycleObserversAcrossReplacementHandles(t *testing.T, oldScope, replacementScope string) {
	t.Helper()
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "cross-handle lifecycle order"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	lifecycleMu := &sync.Mutex{}
	durableOrder := []string{}
	oldBacking := &crossHandleLifecycleBacking{
		MemStore:           base,
		scope:              oldScope,
		lifecycleMu:        lifecycleMu,
		closeLeaseReleased: make(chan struct{}),
		durableOrder:       &durableOrder,
	}
	newBacking := &crossHandleLifecycleBacking{
		MemStore:      base,
		scope:         replacementScope,
		lifecycleMu:   lifecycleMu,
		reopenEntered: make(chan struct{}),
		durableOrder:  &durableOrder,
	}

	var (
		notesMu sync.Mutex
		notes   []cacheWriteNotification
	)
	observer := func(eventType, beadID string, payload json.RawMessage) {
		notesMu.Lock()
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
		notesMu.Unlock()
	}
	oldHandle := NewCachingStoreForTest(oldBacking, observer)
	newHandle := NewCachingStoreForTest(newBacking, observer)
	if err := oldHandle.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(old): %v", err)
	}
	if err := newHandle.Prime(context.Background()); err != nil {
		t.Fatalf("Prime(new): %v", err)
	}

	// Pause the old handle after its durable close has returned but before it
	// can install the snapshot or reserve bead.closed. This is the exact reload
	// window where origin/main lets the replacement handle overtake it.
	oldHandle.mu.Lock()
	closeDone := make(chan error, 1)
	go func() {
		_, err := oldHandle.closeWithReasonIfOpen(created.ID, "resolved", true)
		closeDone <- err
	}()
	<-oldBacking.closeLeaseReleased

	reopenDone := make(chan error, 1)
	go func() { reopenDone <- newHandle.Reopen(created.ID) }()
	reopenReachedBacking := mutationReachedBackingBeforeSerialization(
		t,
		oldHandle,
		created.ID,
		newBacking.reopenEntered,
		2,
	)

	oldHandle.mu.Unlock()
	awaitMutationError(t, "old-handle close", closeDone)
	awaitMutationError(t, "replacement-handle reopen", reopenDone)
	if reopenReachedBacking {
		t.Error("replacement reopen reached the backing store before the old handle reserved its close notification")
	}
	if want := []string{"bead.closed", "bead.updated(open)"}; !slices.Equal(durableOrder, want) {
		t.Fatalf("durable mutation order = %v, want %v", durableOrder, want)
	}

	notesMu.Lock()
	gotNotes := slices.Clone(notes)
	notesMu.Unlock()
	if len(gotNotes) != 2 || gotNotes[0].eventType != "bead.closed" || gotNotes[1].eventType != "bead.updated" {
		t.Fatalf("observer notifications = %+v, want bead.closed then bead.updated", gotNotes)
	}
	closed, ok := DecodeBeadEventPayload(gotNotes[0].payload)
	if !ok || closed.ID != created.ID || closed.Status != "closed" {
		t.Fatalf("bead.closed payload = %+v ok=%v, want authoritative closed snapshot", closed, ok)
	}
	reopened, ok := DecodeBeadEventPayload(gotNotes[1].payload)
	if !ok || reopened.ID != created.ID || reopened.Status != "open" {
		t.Fatalf("bead.updated payload = %+v ok=%v, want authoritative open snapshot", reopened, ok)
	}
	fresh, err := newHandle.Get(created.ID)
	if err != nil {
		t.Fatalf("replacement Get: %v", err)
	}
	if fresh.Status != "open" {
		t.Fatalf("replacement cache status = %q, want open", fresh.Status)
	}
}

func TestCachingStoreMutationCoordinationIsScopedByDurableStore(t *testing.T) {
	sharedScope := t.TempDir()
	first := NewCachingStoreForTest(&crossHandleLifecycleBacking{
		MemStore: NewMemStore(),
		scope:    sharedScope,
	}, nil)
	second := NewCachingStoreForTest(&crossHandleLifecycleBacking{
		MemStore: NewMemStore(),
		scope:    sharedScope,
	}, nil)
	independent := NewCachingStoreForTest(&crossHandleLifecycleBacking{
		MemStore: NewMemStore(),
		scope:    t.TempDir(),
	}, nil)
	fallbackBacking := NewMemStore()
	fallbackFirst := NewCachingStoreForTest(fallbackBacking, nil)
	fallbackSecond := NewCachingStoreForTest(fallbackBacking, nil)

	if first.mutationScopeMu != second.mutationScopeMu || first.closeStateMu != second.closeStateMu ||
		first.orderedNotificationMu != second.orderedNotificationMu {
		t.Fatal("caches over the same durable scope did not share mutation coordination")
	}
	if first.mutationScopeMu == independent.mutationScopeMu || first.closeStateMu == independent.closeStateMu ||
		first.orderedNotificationMu == independent.orderedNotificationMu {
		t.Fatal("caches over independent durable scopes unexpectedly shared mutation coordination")
	}
	if fallbackFirst.mutationScopeMu == fallbackSecond.mutationScopeMu ||
		fallbackFirst.closeStateMu == fallbackSecond.closeStateMu ||
		fallbackFirst.orderedNotificationMu == fallbackSecond.orderedNotificationMu {
		t.Fatal("caches without a stable filesystem scope unexpectedly shared mutation coordination")
	}
}
