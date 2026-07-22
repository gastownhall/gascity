package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestMoleculeAutocloseAmbiguousAtomicClosePublishesCommittedTransition(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "ambiguous atomic molecule", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	ambiguousErr := errors.New("connection reset after atomic close commit")
	backing := &moleculeAutocloseCommitThenErrorAtomicStore{
		Store:  base,
		failID: root.ID,
		err:    ambiguousErr,
	}
	rec := events.NewFake()
	rootObserverCloses := 0
	cache := beads.NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		if eventType == events.BeadClosed && beadID == root.ID {
			rootObserverCloses++
		}
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
	})
	if err := cache.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	// Exercise the same optional-capability forwarding layers used by the
	// controller: policy wrapper -> CachingStore -> atomic backing store. This
	// explicit, non-eligibility announcement keeps atomic ambiguity coverage;
	// eligibility-gated autoclose deliberately uses the prepared lifecycle path.
	store := wrapStoreWithBeadPolicies(cache, nil)
	eventStart := len(rec.Events)

	var stdout bytes.Buffer
	announcement := announceClosedMoleculeResult(store, rec, root, moleculeAutocloseReason, &stdout)
	if announcement.lifecycleDone != nil {
		<-announcement.lifecycleDone
	}
	retry := announcement.lifecycleRetryNeeded
	if announcement.lifecycleRetry != nil {
		retry = announcement.lifecycleRetry() || retry
	}

	durable, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if durable.Status != "closed" {
		t.Fatalf("root status = %q, want closed; backing did not reproduce the commit-before-error ambiguity", durable.Status)
	}
	if got := durable.Metadata["close_reason"]; got != moleculeAutocloseReason {
		t.Fatalf("root close_reason = %q, want %q", got, moleculeAutocloseReason)
	}

	lifecycle := append([]events.Event(nil), rec.Events[eventStart:]...)
	if rootObserverCloses != 1 {
		t.Errorf("cache observer bead.closed events = %d, want 1 for the authoritative committed transition", rootObserverCloses)
	}
	if !hasOrderedMoleculeLifecycle(lifecycle) {
		t.Fatalf("lifecycle events = %v, want bead.closed then molecule.resolved from the authoritative committed transition", moleculeLifecycleTypes(lifecycle))
	}
	assertMoleculeAutocloseBeadClosedSnapshot(t, lifecycle, durable, moleculeAutocloseReason)
	if retry {
		t.Fatal("autoclose retry = true, want false after immediate lifecycle publication")
	}
	if got := durable.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; got != "" {
		t.Fatalf("pending lifecycle marker = %q, want absent for an authoritative atomic transition", got)
	}
	if got := durable.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]; got != "" {
		t.Fatalf("durable lifecycle intent = %q, want absent for an authoritative atomic transition", got)
	}
}

type moleculeAutocloseCommitThenErrorAtomicStore struct {
	beads.Store
	failID string
	err    error
}

func (s *moleculeAutocloseCommitThenErrorAtomicStore) CloseWithReasonIfOpen(id, reason string) (beads.CloseTransition, error) {
	closer, ok := beads.CloseTransitionerFor(s.Store)
	if !ok {
		return beads.CloseTransition{}, beads.ErrCloseTransitionUnsupported
	}
	transition, err := closer.CloseWithReasonIfOpen(id, reason)
	if err != nil {
		return transition, err
	}
	if id == s.failID && transition.Transitioned {
		return transition, s.err
	}
	return transition, nil
}

func hasOrderedMoleculeLifecycle(recorded []events.Event) bool {
	types := moleculeLifecycleTypes(recorded)
	return len(types) == 2 && types[0] == events.BeadClosed && types[1] == events.MoleculeResolved
}

func moleculeLifecycleTypes(recorded []events.Event) []string {
	types := make([]string, 0, len(recorded))
	for _, event := range recorded {
		if event.Type == events.BeadClosed || event.Type == events.MoleculeResolved {
			types = append(types, event.Type)
		}
	}
	return types
}
