package beads

import (
	"sync/atomic"
	"testing"
	"time"
)

type listCountingStore struct {
	*MemStore
	lists atomic.Int64
}

func (s *listCountingStore) List(q ListQuery) ([]Bead, error) {
	s.lists.Add(1)
	return s.MemStore.List(q)
}

// A closed reconcile gate skips the periodic full scan without stopping the
// loop; reopening it lets the next due cycle run.
func TestReconcileGateSkipsTheFullScanWhileClosed(t *testing.T) {
	backing := &listCountingStore{MemStore: NewMemStore()}
	var allowed atomic.Bool
	cs := NewCachingStoreForTest(backing, nil)
	WithReconcileGate(allowed.Load)(cs)

	// Never primed, so a reconcile is due.
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load(); got != 0 {
		t.Fatalf("closed gate: backing List called %d time(s), want 0", got)
	}

	allowed.Store(true)
	cs.reconcileIfDue(time.Now())
	if got := backing.lists.Load(); got == 0 {
		t.Fatal("open gate: the due reconcile did not scan the backing store")
	}
}
