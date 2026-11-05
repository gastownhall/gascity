package beads

import (
	"context"
	"errors"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// TestNativeDoltStoreDepRemoveBatchReportsAServedStoreUnsupported (round 3
// X2): a native store served over the v0 wire has no transaction to open, so
// the batch reports ErrDepRemoveBatchUnsupported, the sentinel the clear falls
// back to edge-by-edge removal on, and not a bare *ErrUnsupported it would
// fail the clear with after the marker.
func TestNativeDoltStoreDepRemoveBatchReportsAServedStoreUnsupported(t *testing.T) {
	store := newNativeDoltStoreForTest(&batchOnlyMemStorage{nativeDoltMemStorage: newNativeDoltMemStorage()})
	err := store.DepRemoveBatch([]DepPair{{IssueID: "gc-1", DependsOnID: "gc-2"}})
	if !errors.Is(err, ErrDepRemoveBatchUnsupported) {
		t.Fatalf("DepRemoveBatch on a served store = %v, want ErrDepRemoveBatchUnsupported", err)
	}
}

// TestNativeDepRemoveBatchDeadlineGrowsWithTheBatch (round 3 MUST-1): the
// budget is the flat operation budget plus a second per edge, so a batch at
// the clear's cap has room for a host far slower than the 55-65 ms an edge
// measured.
func TestNativeDepRemoveBatchDeadlineGrowsWithTheBatch(t *testing.T) {
	if got := nativeDepRemoveBatchDeadline(0); got != bdCommandTimeout {
		t.Fatalf("deadline of an empty batch = %v, want the flat %v", got, bdCommandTimeout)
	}
	if got, want := nativeDepRemoveBatchDeadline(250), bdCommandTimeout+250*time.Second; got != want {
		t.Fatalf("deadline of 250 edges = %v, want %v", got, want)
	}
}

// deadlineSpyStorage is the mem double recording the deadline of the context
// a transaction is opened with.
type deadlineSpyStorage struct {
	*nativeDoltMemStorage
	budget time.Duration
}

func (s *deadlineSpyStorage) RunInTransaction(ctx context.Context, _ string, _ func(beadslib.Transaction) error) error {
	if deadline, ok := ctx.Deadline(); ok {
		s.budget = time.Until(deadline)
	}
	return nil
}

// TestNativeDoltStoreDepRemoveBatchOpensItsTransactionWithTheScaledDeadline:
// the batch's transaction runs under the budget its size earns, not the flat
// operation budget a two-thousand-edge removal ran out of.
func TestNativeDoltStoreDepRemoveBatchOpensItsTransactionWithTheScaledDeadline(t *testing.T) {
	spy := &deadlineSpyStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(spy)
	pairs := make([]DepPair, 250)
	for i := range pairs {
		pairs[i] = DepPair{IssueID: "gc-a", DependsOnID: "gc-b"}
	}
	if err := store.DepRemoveBatch(pairs); err != nil {
		t.Fatal(err)
	}
	if want := nativeDepRemoveBatchDeadline(len(pairs)); spy.budget <= bdCommandTimeout || spy.budget > want {
		t.Fatalf("the transaction opened with %v to run, want the scaled %v", spy.budget, want)
	}
}
