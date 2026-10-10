package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// transferCapableBackingStore is a test-only double that implements
// ConditionalAssigneeTransferer over a plain MemStore. MemStore itself never
// grew TransferIfCurrent (only BdStore and the role-backed NativeDoltStore
// did, matching the "FAMILY DIVERGENCE LEDGER" note on
// ConditionalAssignmentReleaser in beads.go — not every store family picked
// up every conditional-assignee capability), so CachingStore's forwarding
// path has nothing real to delegate to without a double. The precondition
// this double checks (status == in_progress && assignee == fromAssignee)
// mirrors BdStore.TransferIfCurrent's and NativeDoltStore.TransferIfCurrent's
// own contract closely enough to exercise CachingStore's delegate-then-
// refresh behavior, including the precondition-miss path a real backing can
// also take.
type transferCapableBackingStore struct {
	Store
	transferCalls int
}

func (s *transferCapableBackingStore) TransferIfCurrent(id, fromAssignee, toAssignee string) (bool, error) {
	s.transferCalls++
	current, err := s.Get(id)
	if err != nil {
		return false, nil
	}
	if current.Status != "in_progress" || current.Assignee != fromAssignee {
		return false, nil
	}
	assignee := toAssignee
	if err := s.Update(id, UpdateOpts{Assignee: &assignee}); err != nil {
		return false, err
	}
	return true, nil
}

func TestCachingStoreTransferIfCurrentDelegatesAndRefreshesCache(t *testing.T) {
	t.Parallel()

	status := "in_progress"
	backing := &transferCapableBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "task", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	events = nil

	moved, err := cache.TransferIfCurrent(bead.ID, "worker-1", "worker-2")
	if err != nil {
		t.Fatalf("TransferIfCurrent: %v", err)
	}
	if !moved {
		t.Fatal("TransferIfCurrent moved = false, want true")
	}
	if backing.transferCalls != 1 {
		t.Fatalf("backing TransferIfCurrent calls = %d, want 1", backing.transferCalls)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("cache Get: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-2" {
		t.Fatalf("cached bead = %+v, want in_progress and reassigned to worker-2", got)
	}
	if !stringSliceContains(events, "bead.updated:"+bead.ID) {
		t.Fatalf("events = %v, want bead.updated for the transferred bead", events)
	}
}

func TestCachingStoreTransferIfCurrentPreconditionMissAndUnsupportedNeverTouchTheCache(t *testing.T) {
	t.Parallel()

	t.Run("precondition miss", func(t *testing.T) {
		t.Parallel()
		backing := &transferCapableBackingStore{Store: NewMemStore()}
		bead, err := backing.Create(Bead{Title: "task", Assignee: "holder"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		status := "in_progress"
		if err := backing.Update(bead.ID, UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("Update: %v", err)
		}

		var events []string
		cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
			events = append(events, eventType+":"+beadID)
		})
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		events = nil

		moved, err := cache.TransferIfCurrent(bead.ID, "someone-else", "worker-2")
		if err != nil {
			t.Fatalf("TransferIfCurrent error = %v, want nil on a precondition miss", err)
		}
		if moved {
			t.Fatal("TransferIfCurrent moved = true on a precondition miss")
		}
		if len(events) != 0 {
			t.Fatalf("events = %v, want none for a precondition-missed transfer", events)
		}
	})

	t.Run("backing lacks TransferIfCurrent", func(t *testing.T) {
		t.Parallel()
		backing := NewMemStore() // no TransferIfCurrent(string,string,string) method
		cache := NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}

		moved, err := cache.TransferIfCurrent("nonexistent", "worker-1", "worker-2")
		if moved {
			t.Fatal("TransferIfCurrent moved = true against a store with no TransferIfCurrent capability")
		}
		if !errors.Is(err, ErrConditionalTransferUnsupported) {
			t.Fatalf("err = %v, want ErrConditionalTransferUnsupported", err)
		}
	})
}
