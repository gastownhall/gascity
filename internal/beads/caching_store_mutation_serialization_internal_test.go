package beads

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// cacheMutationRaceBacking exposes deterministic post-commit windows around
// the backing operations used by the cache mutation serialization tests.
// Every channel is optional and each configured operation is invoked once per
// test.
type cacheMutationRaceBacking struct {
	*MemStore

	createCommitted chan struct{}
	releaseCreate   chan struct{}
	closeEntered    chan struct{}
	closeCommitted  chan struct{}
	releaseClose    chan struct{}
	updateEntered   chan struct{}
	deleteEntered   chan struct{}
	txCommitted     chan struct{}
	releaseTx       chan struct{}
}

func (s *cacheMutationRaceBacking) Create(b Bead) (Bead, error) {
	created, err := s.MemStore.Create(b)
	closeOptionalTestChannel(s.createCommitted)
	if s.releaseCreate != nil {
		<-s.releaseCreate
	}
	return created, err
}

func (s *cacheMutationRaceBacking) CloseWithReasonIfOpen(id, reason string) (CloseTransition, error) {
	closeOptionalTestChannel(s.closeEntered)
	transition, err := s.MemStore.CloseWithReasonIfOpen(id, reason)
	closeOptionalTestChannel(s.closeCommitted)
	if s.releaseClose != nil {
		<-s.releaseClose
	}
	return transition, err
}

func (s *cacheMutationRaceBacking) Update(id string, opts UpdateOpts) error {
	closeOptionalTestChannel(s.updateEntered)
	return s.MemStore.Update(id, opts)
}

func (s *cacheMutationRaceBacking) DeleteIfMatch(id string, expectedRevision int64) error {
	closeOptionalTestChannel(s.deleteEntered)
	return s.MemStore.DeleteIfMatch(id, expectedRevision)
}

func (s *cacheMutationRaceBacking) Tx(commitMsg string, fn func(Tx) error) error {
	err := s.MemStore.Tx(commitMsg, fn)
	closeOptionalTestChannel(s.txCommitted)
	if s.releaseTx != nil {
		<-s.releaseTx
	}
	return err
}

func closeOptionalTestChannel(ch chan struct{}) {
	if ch != nil {
		close(ch)
	}
}

// mutationReachedBackingBeforeSerialization returns true when reached closes
// before the contender joins the target's per-ID serialization queue. It lets
// the regression drive both the buggy interleaving and the fixed interleaving
// without a wall-clock sleep.
func mutationReachedBackingBeforeSerialization(t *testing.T, cache *CachingStore, id string, reached <-chan struct{}, queuedRefs int) bool {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	for {
		select {
		case <-reached:
			return true
		default:
		}

		cache.closeStateMu.Lock()
		entry := cache.closeStateLocks[id]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		cache.closeStateMu.Unlock()
		if refs >= queuedRefs {
			return false
		}

		select {
		case <-timer.C:
			t.Fatalf("mutation neither reached backing nor joined serialization for %s", id)
		default:
			runtime.Gosched()
		}
	}
}

// unknownMutationReachedBackingBeforeSerialization returns true when an
// unknown-scope mutation reaches its backing commit before it queues on the
// global mutation scope. The waiter count makes the fixed blocked state
// observable without a wall-clock sleep.
func unknownMutationReachedBackingBeforeSerialization(t *testing.T, cache *CachingStore, reached <-chan struct{}) bool {
	t.Helper()
	timer := time.NewTimer(testutil.GoroutineRaceTimeout)
	defer timer.Stop()
	for {
		select {
		case <-reached:
			return true
		default:
		}

		cache.closeStateMu.Lock()
		waiters := cache.unknownMutationWaiters
		cache.closeStateMu.Unlock()
		if waiters > 0 {
			return false
		}

		select {
		case <-timer.C:
			t.Fatal("unknown-scope mutation neither reached backing nor joined serialization")
		default:
			runtime.Gosched()
		}
	}
}

func awaitMutationError(t *testing.T, op string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not complete", op)
	}
}

func assertCacheAndBackingStatus(t *testing.T, cache *CachingStore, backing Store, id, want string) {
	t.Helper()
	for name, store := range map[string]Store{"cache": cache, "backing": backing} {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", name, err)
		}
		if got.Status != want {
			t.Fatalf("%s status = %q, want %q", name, got.Status, want)
		}
	}
}

func TestCachingStoreCloseCannotOverwriteConcurrentUpdate(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "close-update race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheMutationRaceBacking{
		MemStore:       base,
		closeCommitted: make(chan struct{}),
		releaseClose:   make(chan struct{}),
		updateEntered:  make(chan struct{}),
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() {
		_, err := cache.CloseWithReasonIfOpen(created.ID, "racing close")
		closeDone <- err
	}()
	select {
	case <-backing.closeCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("close did not commit")
	}

	active := "in_progress"
	updateDone := make(chan error, 1)
	go func() { updateDone <- cache.Update(created.ID, UpdateOpts{Status: &active}) }()
	if mutationReachedBackingBeforeSerialization(t, cache, created.ID, backing.updateEntered, 2) {
		awaitMutationError(t, "Update", updateDone)
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
	} else {
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
		awaitMutationError(t, "Update", updateDone)
	}

	assertCacheAndBackingStatus(t, cache, base, created.ID, active)
}

func TestCachingStoreCloseCannotOverwriteConcurrentAppliedEvent(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "close-event race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheMutationRaceBacking{
		MemStore:       base,
		closeCommitted: make(chan struct{}),
		releaseClose:   make(chan struct{}),
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() {
		_, err := cache.CloseWithReasonIfOpen(created.ID, "racing close")
		closeDone <- err
	}()
	select {
	case <-backing.closeCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("close did not commit")
	}

	active := "in_progress"
	if err := base.Update(created.ID, UpdateOpts{Status: &active}); err != nil {
		t.Fatalf("external Update: %v", err)
	}
	fresh, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("external Get: %v", err)
	}
	payload, err := json.Marshal(fresh)
	if err != nil {
		t.Fatalf("Marshal event: %v", err)
	}
	applyDone := make(chan struct{})
	go func() {
		cache.ApplyEvent("bead.updated", payload)
		close(applyDone)
	}()
	if mutationReachedBackingBeforeSerialization(t, cache, created.ID, applyDone, 2) {
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
	} else {
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
		select {
		case <-applyDone:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("ApplyEvent did not complete")
		}
	}

	assertCacheAndBackingStatus(t, cache, base, created.ID, active)
}

func TestCachingStoreCloseCannotResurrectConcurrentConditionalDelete(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "close-delete race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheMutationRaceBacking{
		MemStore:       base,
		closeCommitted: make(chan struct{}),
		releaseClose:   make(chan struct{}),
		deleteEntered:  make(chan struct{}),
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() {
		_, err := cache.CloseWithReasonIfOpen(created.ID, "racing close")
		closeDone <- err
	}()
	select {
	case <-backing.closeCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("close did not commit")
	}
	closed, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get committed close: %v", err)
	}

	deleteDone := make(chan error, 1)
	go func() { deleteDone <- cache.DeleteIfMatch(created.ID, closed.Revision) }()
	if mutationReachedBackingBeforeSerialization(t, cache, created.ID, backing.deleteEntered, 2) {
		awaitMutationError(t, "DeleteIfMatch", deleteDone)
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
	} else {
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
		awaitMutationError(t, "DeleteIfMatch", deleteDone)
	}

	if _, err := base.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("backing Get error = %v, want ErrNotFound", err)
	}
	if _, err := cache.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cache Get error = %v, want ErrNotFound", err)
	}
}

func TestCachingStoreCloseCannotOverwriteConcurrentTransaction(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "close-tx race"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheMutationRaceBacking{
		MemStore:       base,
		closeCommitted: make(chan struct{}),
		releaseClose:   make(chan struct{}),
		txCommitted:    make(chan struct{}),
		releaseTx:      make(chan struct{}),
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closeDone := make(chan error, 1)
	go func() {
		_, err := cache.CloseWithReasonIfOpen(created.ID, "racing close")
		closeDone <- err
	}()
	select {
	case <-backing.closeCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("close did not commit")
	}

	active := "in_progress"
	txDone := make(chan error, 1)
	go func() {
		txDone <- cache.Tx("concurrent active update", func(tx Tx) error {
			return tx.Update(created.ID, UpdateOpts{Status: &active})
		})
	}()
	if unknownMutationReachedBackingBeforeSerialization(t, cache, backing.txCommitted) {
		close(backing.releaseTx)
		awaitMutationError(t, "Tx", txDone)
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
	} else {
		close(backing.releaseClose)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
		select {
		case <-backing.txCommitted:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("Tx did not commit after close released")
		}
		close(backing.releaseTx)
		awaitMutationError(t, "Tx", txDone)
	}

	assertCacheAndBackingStatus(t, cache, base, created.ID, active)
}

func TestCachingStoreTransactionNotificationPrecedesLaterClose(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "tx-close order"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	backing := &cacheMutationRaceBacking{
		MemStore:     base,
		closeEntered: make(chan struct{}),
		txCommitted:  make(chan struct{}),
		releaseTx:    make(chan struct{}),
	}
	var (
		notesMu sync.Mutex
		notes   []string
	)
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		if beadID != created.ID {
			return
		}
		notesMu.Lock()
		notes = append(notes, eventType)
		notesMu.Unlock()
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	active := "in_progress"
	txDone := make(chan error, 1)
	go func() {
		txDone <- cache.Tx("active before close", func(tx Tx) error {
			return tx.Update(created.ID, UpdateOpts{Status: &active})
		})
	}()
	select {
	case <-backing.txCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Tx did not commit")
	}

	closeDone := make(chan error, 1)
	go func() {
		_, err := cache.CloseWithReasonIfOpen(created.ID, "after tx")
		closeDone <- err
	}()
	if mutationReachedBackingBeforeSerialization(t, cache, created.ID, backing.closeEntered, 1) {
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
		close(backing.releaseTx)
		awaitMutationError(t, "Tx", txDone)
	} else {
		close(backing.releaseTx)
		awaitMutationError(t, "Tx", txDone)
		awaitMutationError(t, "CloseWithReasonIfOpen", closeDone)
	}

	notesMu.Lock()
	gotNotes := slices.Clone(notes)
	notesMu.Unlock()
	wantNotes := []string{"bead.updated", "bead.closed"}
	if !slices.Equal(gotNotes, wantNotes) {
		t.Fatalf("notification order = %v, want %v", gotNotes, wantNotes)
	}
	assertCacheAndBackingStatus(t, cache, base, created.ID, "closed")
}

func TestCachingStoreCreateAndReconcileEmitCreatedOnce(t *testing.T) {
	base := NewMemStore()
	backing := &cacheMutationRaceBacking{
		MemStore:        base,
		createCommitted: make(chan struct{}),
		releaseCreate:   make(chan struct{}),
	}
	var (
		notesMu sync.Mutex
		notes   []string
	)
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		notesMu.Lock()
		notes = append(notes, eventType+":"+beadID)
		notesMu.Unlock()
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime empty cache: %v", err)
	}

	type createResult struct {
		bead Bead
		err  error
	}
	createDone := make(chan createResult, 1)
	go func() {
		created, err := cache.Create(Bead{Title: "create racing reconcile"})
		createDone <- createResult{bead: created, err: err}
	}()
	select {
	case <-backing.createCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Create did not commit")
	}

	mergeCommitted := make(chan struct{})
	releaseReservation := make(chan struct{})
	cache.reconcileAfterMergeBeforeNotificationReservationForTest = func() {
		close(mergeCommitted)
		<-releaseReservation
	}
	reconcileDone := make(chan struct{})
	go func() {
		cache.runReconciliation()
		close(reconcileDone)
	}()

	if unknownMutationReachedBackingBeforeSerialization(t, cache, mergeCommitted) {
		// Buggy path: reconciliation shares the read scope with Create, so it
		// observes and notifies the committed row before Create reserves the
		// generated ID. Finish reconciliation first to expose the duplicate.
		close(releaseReservation)
		select {
		case <-reconcileDone:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("reconciliation did not complete")
		}
		close(backing.releaseCreate)
	} else {
		// Fixed path: reconciliation waits for Create's read scope through its
		// cache merge and notification reservation. Its stale snapshot is then
		// rejected by Create's sequence fence.
		close(backing.releaseCreate)
		select {
		case <-mergeCommitted:
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("reconciliation did not merge after Create completed")
		}
		close(releaseReservation)
	}

	var result createResult
	select {
	case result = <-createDone:
		if result.err != nil {
			t.Fatalf("Create: %v", result.err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Create did not complete")
	}
	select {
	case <-reconcileDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reconciliation did not complete")
	}

	notesMu.Lock()
	gotNotes := slices.Clone(notes)
	notesMu.Unlock()
	wantNotes := []string{"bead.created:" + result.bead.ID}
	if !slices.Equal(gotNotes, wantNotes) {
		t.Fatalf("notifications = %v, want %v", gotNotes, wantNotes)
	}
}

func TestCachingStoreReconcileNotificationPrecedesLaterClose(t *testing.T) {
	base := NewMemStore()
	backing := &cacheMutationRaceBacking{
		MemStore:     base,
		closeEntered: make(chan struct{}),
	}
	var (
		notesMu sync.Mutex
		notes   []string
	)
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		notesMu.Lock()
		notes = append(notes, eventType+":"+beadID)
		notesMu.Unlock()
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime empty cache: %v", err)
	}

	created, err := base.Create(Bead{Title: "external create before reconcile"})
	if err != nil {
		t.Fatalf("external Create: %v", err)
	}
	mergeCommitted := make(chan struct{})
	releaseReservation := make(chan struct{})
	cache.reconcileAfterMergeBeforeNotificationReservationForTest = func() {
		close(mergeCommitted)
		<-releaseReservation
	}

	reconcileDone := make(chan struct{})
	go func() {
		cache.runReconciliation()
		close(reconcileDone)
	}()
	select {
	case <-mergeCommitted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reconciliation did not commit its cache merge")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- cache.Close(created.ID) }()
	if mutationReachedBackingBeforeSerialization(t, cache, created.ID, backing.closeEntered, 1) {
		// Buggy path: reconciliation holds no per-ID reservation, so the newer
		// close reaches backing and delivers before stale bead.created.
		awaitMutationError(t, "Close", closeDone)
		close(releaseReservation)
	} else {
		// Fixed path: the close is queued behind reconciliation until its
		// notification has reserved the same-ID ordered delivery queue.
		close(releaseReservation)
		awaitMutationError(t, "Close", closeDone)
	}
	select {
	case <-reconcileDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("reconciliation did not complete")
	}

	notesMu.Lock()
	gotNotes := slices.Clone(notes)
	notesMu.Unlock()
	wantNotes := []string{
		"bead.created:" + created.ID,
		"bead.closed:" + created.ID,
	}
	if !slices.Equal(gotNotes, wantNotes) {
		t.Fatalf("notification order = %v, want %v", gotNotes, wantNotes)
	}
	assertCacheAndBackingStatus(t, cache, base, created.ID, "closed")
}
