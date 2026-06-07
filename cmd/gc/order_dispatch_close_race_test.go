package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// latchedCloseStore models the one-way close latch of a native bead store
// (internal/beads/native_dolt_store.go: once CloseStore nils the storage
// handle, acquireStorage returns ErrStoreClosed forever — there is no reopen).
// It records the first data operation attempted AFTER CloseStore ran, which is
// exactly the use-after-close gascity#3157 reports. beads.MemStore hides the
// bug because its handle has no CloseStore method, so closeBeadStoreHandle is a
// no-op for MemStore-backed tests; this double makes the latch observable.
type latchedCloseStore struct {
	beads.Store

	mu              sync.Mutex
	closed          bool
	useAfterCloseOp string
	closedCh        chan struct{}
	closeErr        error // mirrors NativeDoltStore.CloseStore returning storage.Close()'s error
}

func newLatchedCloseStore() *latchedCloseStore {
	return &latchedCloseStore{Store: beads.NewMemStore(), closedCh: make(chan struct{})}
}

// guard records the first post-close operation and mimics the native store's
// ErrStoreClosed once the handle has been closed.
func (s *latchedCloseStore) guard(op string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		if s.useAfterCloseOp == "" {
			s.useAfterCloseOp = op
		}
		return fmt.Errorf("native Dolt store: %w", beads.ErrStoreClosed)
	}
	return nil
}

func (s *latchedCloseStore) usedAfterClose() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.useAfterCloseOp, s.useAfterCloseOp != ""
}

func (s *latchedCloseStore) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// CloseStore latches the store closed exactly like NativeDoltStore.CloseStore.
func (s *latchedCloseStore) CloseStore() error {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if !already {
		close(s.closedCh)
	}
	return s.closeErr
}

func (s *latchedCloseStore) Create(b beads.Bead) (beads.Bead, error) {
	if err := s.guard("Create"); err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Create(b)
}

func (s *latchedCloseStore) Get(id string) (beads.Bead, error) {
	if err := s.guard("Get"); err != nil {
		return beads.Bead{}, err
	}
	return s.Store.Get(id)
}

func (s *latchedCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if err := s.guard("Update"); err != nil {
		return err
	}
	return s.Store.Update(id, opts)
}

func (s *latchedCloseStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if err := s.guard("CloseAll"); err != nil {
		return 0, err
	}
	return s.Store.CloseAll(ids, metadata)
}

func (s *latchedCloseStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if err := s.guard("List"); err != nil {
		return nil, err
	}
	return s.Store.List(query)
}

// TestOrderDispatchDoesNotCloseStoreWhileDispatchInFlight reproduces
// gascity#3157: memoryOrderDispatcher.dispatch opens per-tick bead stores and
// closes them in a defer when the tick returns (order_dispatch.go:412-418), but
// the async dispatchOne goroutine launched that same tick (:597-598) still holds
// the handle. On a native store (one-way close latch) the goroutine's post-tick
// writes — Update at :1127 and the deferred CloseAll at :933 — hit the closed
// handle and fail with "native Dolt store: bead store closed".
//
// The test is deterministic: execRun parks the in-flight goroutine until the
// test releases it AFTER dispatch() has returned. For a cooldown exec order,
// dispatchExec performs no store operation before execRun, so the goroutine's
// first store touch is forced to occur after the per-tick defer has run.
func TestOrderDispatchDoesNotCloseStoreWhileDispatchInFlight(t *testing.T) {
	store := newLatchedCloseStore()

	released := make(chan struct{})
	blockingExec := func(context.Context, string, string, []string) ([]byte, error) {
		<-released
		return []byte("ok"), nil
	}

	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:     "race-exec",
		Trigger:  "cooldown",
		Interval: "5m",
		Exec:     "true",
	}}, store, nil, blockingExec, events.Discard)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)

	mad.dispatch(context.Background(), t.TempDir(), time.Now())
	// dispatch() has returned. With the bug, its defer already closed the store
	// handle the in-flight goroutine still holds. Release the goroutine so it
	// performs its tracking-bead writes against that handle.
	close(released)

	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !mad.drain(drainCtx) {
		t.Fatal("drain timed out waiting for in-flight dispatchOne to finish")
	}

	if op, used := store.usedAfterClose(); used {
		t.Fatalf("dispatchOne issued %s on the order store after the per-tick defer closed it (gascity#3157 use-after-close race)", op)
	}

	// The fix must still close the per-tick store once the in-flight goroutine
	// finishes — deferring the close must not silently turn into a handle leak.
	deadline := time.Now().Add(2 * time.Second)
	for !store.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("per-tick store was never closed after dispatch drained (handle leak)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
