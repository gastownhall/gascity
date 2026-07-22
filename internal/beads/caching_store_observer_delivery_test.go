package beads

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestCachingStoreCloseObserverDeliveryIsImmediateAfterSynchronousPublish(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "immediate observer delivery"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	observerReturned := false
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, _ json.RawMessage) {
		if eventType == "bead.closed" && beadID == created.ID {
			observerReturned = true
		}
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	transition, err := cache.CloseWithReasonIfOpen(created.ID, "immediate delivery")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.ObserverNotified {
		t.Fatal("ObserverNotified = false after synchronous observer publication")
	}
	if transition.ObserverDelivery == nil {
		t.Fatal("ObserverDelivery = nil, want exact close-notification receipt")
	}
	if !observerReturned {
		t.Fatal("CloseWithReasonIfOpen returned before its synchronous observer")
	}

	afterCalls := 0
	transition.ObserverDelivery.AfterDelivery(func() { afterCalls++ })
	if afterCalls != 1 {
		t.Fatalf("AfterDelivery callbacks = %d, want immediate single invocation", afterCalls)
	}
}

func TestCacheObserverDeliveryRegistrationRacesCompletionOnce(t *testing.T) {
	for i := 0; i < 100; i++ {
		delivery := &cacheObserverDelivery{}
		start := make(chan struct{})
		var calls atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			delivery.AfterDelivery(func() { calls.Add(1) })
		}()
		go func() {
			defer wg.Done()
			<-start
			delivery.markDelivered()
		}()
		close(start)
		wg.Wait()

		if got := calls.Load(); got != 1 {
			t.Fatalf("iteration %d: callback invocations = %d, want 1", i, got)
		}
	}
}

func TestCachingStoreBeadObserverBarrierOrdersWithoutMutating(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "observer barrier target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	observerEntered := make(chan struct{})
	releaseObserver := make(chan struct{})
	var once sync.Once
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, _ json.RawMessage) {
		if eventType == "bead.updated" && beadID == created.ID {
			once.Do(func() {
				close(observerEntered)
				<-releaseObserver
			})
		}
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() { writeDone <- cache.SetMetadata(created.ID, "prior", "queued") }()
	select {
	case <-observerEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior observer did not enter")
	}
	beforeBarrier, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before barrier: %v", err)
	}

	delivery := cache.BeadObserverBarrier(created.ID)
	if delivery == nil {
		t.Fatal("BeadObserverBarrier returned nil delivery")
	}
	barrierDone := make(chan struct{})
	delivery.AfterDelivery(func() { close(barrierDone) })
	select {
	case <-barrierDone:
		t.Fatal("barrier completed before the prior observer returned")
	default:
	}

	close(releaseObserver)
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("prior write did not finish")
	}
	select {
	case <-barrierDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("barrier did not complete after the prior observer")
	}

	afterBarrier, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after barrier: %v", err)
	}
	if afterBarrier.Revision != beforeBarrier.Revision {
		t.Fatalf("barrier changed revision from %d to %d", beforeBarrier.Revision, afterBarrier.Revision)
	}
	if afterBarrier.Metadata["prior"] != "queued" {
		t.Fatalf("metadata after barrier = %#v, want prior=queued", afterBarrier.Metadata)
	}
}
