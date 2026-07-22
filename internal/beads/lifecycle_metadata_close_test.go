package beads

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
)

func TestLifecycleMetadataTransactionCloseReturnsAuthoritativeState(t *testing.T) {
	store := NewMemStore()
	root, err := store.Create(Bead{Title: "transaction close", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const reason = "transaction-scoped lifecycle close"

	err = WithLifecycleMetadataTransaction(store, root.ID, func(tx LifecycleMetadataTransaction) error {
		if err := tx.SetMetadata("close_reason", reason); err != nil {
			return err
		}
		result, err := CloseWithinLifecycleMetadataTransaction(tx, reason)
		if err != nil {
			return err
		}
		if result.Before.ID != root.ID || result.Before.Status != "open" {
			t.Fatalf("before = %+v, want open root %q", result.Before, root.ID)
		}
		if !result.AuthoritativeClosed(root.ID) || !result.Transitioned || !result.CloseSucceeded {
			t.Fatalf("close result = %+v, want acknowledged authoritative transition", result)
		}
		if result.After.Metadata["close_reason"] != reason {
			t.Fatalf("after close_reason = %q, want %q", result.After.Metadata["close_reason"], reason)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
	}
}

func TestCachingLifecycleTransactionCloseDefersObserversUntilLeaseRelease(t *testing.T) {
	backing := NewMemStore()
	root, err := backing.Create(Bead{Title: "queued transaction close", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var (
		insideLease atomic.Bool
		mu          sync.Mutex
		eventTypes  []string
	)
	observerDuringLease := atomic.Bool{}
	cache := NewCachingStoreForTest(backing, func(eventType, _ string, _ json.RawMessage) {
		if insideLease.Load() {
			observerDuringLease.Store(true)
		}
		mu.Lock()
		eventTypes = append(eventTypes, eventType)
		mu.Unlock()
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	receiptDone := make(chan struct{})
	err = WithLifecycleMetadataTransaction(cache, root.ID, func(tx LifecycleMetadataTransaction) error {
		insideLease.Store(true)
		defer insideLease.Store(false)
		if err := tx.SetMetadata("close_reason", "queued transaction-scoped close"); err != nil {
			return err
		}
		result, err := CloseWithinLifecycleMetadataTransaction(tx, "queued transaction-scoped close")
		if err != nil {
			return err
		}
		if !result.AuthoritativeClosed(root.ID) || result.ObserverDelivery == nil {
			t.Fatalf("close result = %+v, want authoritative row and receipt", result)
		}
		result.ObserverDelivery.AfterDelivery(func() { close(receiptDone) })
		select {
		case <-receiptDone:
			t.Fatal("close receipt completed while lifecycle lease was held")
		default:
		}
		mu.Lock()
		defer mu.Unlock()
		if len(eventTypes) != 0 {
			t.Fatalf("observer events inside lifecycle lease = %v, want none", eventTypes)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
	}
	if observerDuringLease.Load() {
		t.Fatal("cache observer ran while lifecycle lease was held")
	}
	select {
	case <-receiptDone:
	default:
		t.Fatal("close receipt did not complete after lifecycle lease release")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(eventTypes) != 1 || eventTypes[0] != "bead.updated" {
		t.Fatalf("observer events = %v, want only prepared metadata update", eventTypes)
	}
}
