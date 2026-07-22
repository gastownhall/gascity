package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestPublishPendingMoleculeLifecycleDoesNotEmitStaleCloseAfterReopen(t *testing.T) {
	base := beads.NewMemStore()
	store := &lifecyclePublicationRaceStore{
		MemStore:                   base,
		secondTransactionAttempted: make(chan struct{}),
		reopenAttempted:            make(chan struct{}),
	}
	root, intent := seedPendingClosedMolecule(t, store, false, moleculeAutocloseReason, time.Now().UTC())
	recorder := newLifecyclePublicationPauseRecorder()

	publisherA := make(chan moleculeLifecyclePublishResult, 1)
	go func() {
		publisherA <- publishPendingMoleculeLifecycleNow(store, recorder, root.ID, intent.IntentID)
	}()
	waitLifecyclePublicationSignal(t, "publisher A record", recorder.entered)

	publisherB := make(chan moleculeLifecyclePublishResult, 1)
	go func() {
		publisherB <- publishPendingMoleculeLifecycleNow(store, recorder, root.ID, intent.IntentID)
	}()

	emitAuthoritativeOpen := func() error {
		if err := store.Reopen(root.ID); err != nil {
			return fmt.Errorf("reopen root: %w", err)
		}
		opened, err := store.Get(root.ID)
		if err != nil {
			return fmt.Errorf("read reopened root: %w", err)
		}
		payload, err := json.Marshal(opened)
		if err != nil {
			return fmt.Errorf("marshal reopened root: %w", err)
		}
		recorder.Record(events.Event{Type: events.BeadUpdated, Subject: root.ID, Payload: payload})
		return nil
	}

	var (
		publisherBResult moleculeLifecyclePublishResult
		publisherBDone   bool
		reopenDone       chan error
	)
	select {
	case publisherBResult = <-publisherB:
		publisherBDone = true
		// This is the vulnerable interleaving: publisher B records and clears
		// while publisher A is paused after validating the same intent.
		if err := emitAuthoritativeOpen(); err != nil {
			t.Fatal(err)
		}
	case <-store.secondTransactionAttempted:
		// A fixed publisher owns the lifecycle transaction while recording, so
		// both publisher B and Reopen must wait for that ownership to finish.
		reopenDone = make(chan error, 1)
		go func() { reopenDone <- emitAuthoritativeOpen() }()
		waitLifecyclePublicationSignal(t, "reopen attempt", store.reopenAttempted)
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("publisher B neither completed nor contended for lifecycle ownership")
	}

	recorder.release()
	publisherAResult := waitLifecyclePublicationResult(t, publisherA)
	if !publisherBDone {
		publisherBResult = waitLifecyclePublicationResult(t, publisherB)
	}
	if reopenDone != nil {
		select {
		case err := <-reopenDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("reopen did not finish after publisher A released lifecycle ownership")
		}
	}
	if publisherAResult.retry || publisherBResult.retry {
		t.Fatalf("publication retries = publisher A:%t publisher B:%t, want both complete", publisherAResult.retry, publisherBResult.retry)
	}

	recorded := recorder.snapshot()
	closedCount, resolvedCount, openAt := 0, 0, -1
	for index, event := range recorded {
		switch event.Type {
		case events.BeadClosed:
			closedCount++
		case events.MoleculeResolved:
			resolvedCount++
		case events.BeadUpdated:
			var snapshot beads.Bead
			if err := json.Unmarshal(event.Payload, &snapshot); err == nil && snapshot.ID == root.ID && snapshot.Status == "open" {
				openAt = index
			}
		}
	}
	if closedCount != 1 || resolvedCount != 1 {
		t.Fatalf("lifecycle event counts = bead.closed:%d molecule.resolved:%d, want one each: %v", closedCount, resolvedCount, lifecycleEventTypes(recorded))
	}
	if openAt < 0 {
		t.Fatalf("authoritative open event missing: %v", lifecycleEventTypes(recorded))
	}
	for _, event := range recorded[openAt+1:] {
		if event.Type == events.BeadClosed || event.Type == events.MoleculeResolved {
			t.Fatalf("late stale lifecycle event %q after authoritative open: %v", event.Type, lifecycleEventTypes(recorded))
		}
	}

	projector := runproj.NewProjector()
	projector.Apply(recorded)
	projected := projector.Beads()
	if len(projected) != 1 || projected[0].ID != root.ID || projected[0].Status != "open" {
		t.Fatalf("projected beads = %+v, want root %s open after authoritative reopen", projected, root.ID)
	}
}

type lifecyclePublicationRaceStore struct {
	*beads.MemStore

	txMu                       sync.Mutex
	transactionCount           atomic.Uint32
	secondTransactionAttempted chan struct{}
	reopenAttempted            chan struct{}
	reopenAttemptOnce          sync.Once
}

func (s *lifecyclePublicationRaceStore) WithLifecycleMetadataTransaction(id string, fn func(beads.LifecycleMetadataTransaction) error) error {
	if s.transactionCount.Add(1) == 2 {
		close(s.secondTransactionAttempted)
	}
	s.txMu.Lock()
	defer s.txMu.Unlock()
	return beads.WithLifecycleMetadataTransaction(s.MemStore, id, fn)
}

func (s *lifecyclePublicationRaceStore) Reopen(id string) error {
	s.reopenAttemptOnce.Do(func() { close(s.reopenAttempted) })
	s.txMu.Lock()
	defer s.txMu.Unlock()
	return s.MemStore.Reopen(id)
}

type lifecyclePublicationPauseRecorder struct {
	mu sync.Mutex

	armed       atomic.Bool
	entered     chan struct{}
	releaseCh   chan struct{}
	releaseOnce sync.Once
	events      []events.Event
}

func newLifecyclePublicationPauseRecorder() *lifecyclePublicationPauseRecorder {
	recorder := &lifecyclePublicationPauseRecorder{
		entered:   make(chan struct{}),
		releaseCh: make(chan struct{}),
	}
	recorder.armed.Store(true)
	return recorder
}

func (r *lifecyclePublicationPauseRecorder) Record(event events.Event) {
	if event.Type == events.BeadClosed && r.armed.CompareAndSwap(true, false) {
		close(r.entered)
		<-r.releaseCh
	}
	r.mu.Lock()
	event.Seq = uint64(len(r.events) + 1)
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *lifecyclePublicationPauseRecorder) RecordDurably(batch ...events.Event) error { //nolint:unparam // error return satisfies events.DurableRecorder; this synchronization spy always succeeds
	for _, event := range batch {
		r.Record(event)
	}
	return nil
}

func (r *lifecyclePublicationPauseRecorder) release() {
	r.releaseOnce.Do(func() { close(r.releaseCh) })
}

func (r *lifecyclePublicationPauseRecorder) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

func waitLifecyclePublicationSignal(t *testing.T, name string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func waitLifecyclePublicationResult(t *testing.T, done <-chan moleculeLifecyclePublishResult) moleculeLifecyclePublishResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for lifecycle publisher")
		return moleculeLifecyclePublishResult{}
	}
}
