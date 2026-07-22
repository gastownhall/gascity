package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type updateRefreshErrorStore struct {
	Store
	getErr  error
	updated bool
}

type updatePostCommitNotFoundStore struct {
	Store
	updated bool
}

type updatePreReadErrorStore struct {
	Store
	getErr      error
	updateCalls int
}

type capabilityStrippedUpdateStore struct{ Store }

type reopenDuringLegacyUpdateStore struct{ *MemStore }

type updateTransitionCommitErrorStore struct {
	*MemStore
	err            error
	stripOwnership bool
}

func (s *updatePreReadErrorStore) Get(string) (Bead, error) {
	return Bead{}, s.getErr
}

func (s *updatePreReadErrorStore) Update(id string, opts UpdateOpts) error {
	s.updateCalls++
	return s.Store.Update(id, opts)
}

func (s *updatePostCommitNotFoundStore) Update(id string, opts UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	s.updated = true
	return nil
}

func (s *updatePostCommitNotFoundStore) Get(id string) (Bead, error) {
	if s.updated {
		return Bead{}, ErrNotFound
	}
	return s.Store.Get(id)
}

func (s *updateRefreshErrorStore) Get(id string) (Bead, error) {
	if s.updated && s.getErr != nil {
		return Bead{}, s.getErr
	}
	return s.Store.Get(id)
}

func (s *updateRefreshErrorStore) Update(id string, opts UpdateOpts) error {
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	s.updated = true
	return nil
}

// Update models the exact legacy interleaving that loses transition ownership:
// CachingStore reads a closed row, a peer reopens it, then this close update
// commits. The backing-owned transition method observes the reopen itself.
func (s *reopenDuringLegacyUpdateStore) Update(id string, opts UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" {
		if err := s.Reopen(id); err != nil {
			return err
		}
	}
	return s.MemStore.Update(id, opts)
}

func (s *reopenDuringLegacyUpdateStore) UpdateWithTransition(id string, opts UpdateOpts) (UpdateTransition, error) {
	if err := s.Reopen(id); err != nil {
		return UpdateTransition{}, err
	}
	before, err := s.Get(id)
	if err != nil {
		return UpdateTransition{}, err
	}
	if err := s.MemStore.Update(id, opts); err != nil {
		return UpdateTransition{Before: before}, err
	}
	after, err := s.Get(id)
	if err != nil {
		return UpdateTransition{Before: before}, err
	}
	return UpdateTransition{
		Before:               before,
		After:                after,
		TransitionedToClosed: before.Status != "closed" && after.Status == "closed",
	}, nil
}

func (s *updateTransitionCommitErrorStore) UpdateWithTransition(id string, opts UpdateOpts) (UpdateTransition, error) {
	transition, err := s.MemStore.UpdateWithTransition(id, opts)
	if err != nil {
		return transition, err
	}
	if s.stripOwnership {
		transition.TransitionedToClosed = false
	}
	return transition, s.err
}

func TestCachingStoreUpdateStatusClosedEmitsBeadClosed(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "status-close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed := "closed"
	if err := cache.Update(created.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update(status=closed): %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one bead.closed for the committed status transition", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.ID != created.ID || payload.Status != "closed" {
		t.Fatalf("bead.closed payload = %+v ok=%v, want authoritative closed snapshot", payload, ok)
	}
}

func TestCachingStoreUpdateOnAlreadyClosedBeadEmitsBeadUpdated(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "closed metadata target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Update(created.ID, UpdateOpts{Metadata: map[string]string{"audit": "retained"}}); err != nil {
		t.Fatalf("Update(metadata): %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one bead.updated for an already-closed metadata edit", notes)
	}
}

func TestCachingStoreUnprimedUpdateOnAlreadyClosedBeadEmitsBeadUpdated(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "unprimed closed target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	closed := "closed"
	if err := cache.Update(created.ID, UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{"audit": "retained"},
	}); err != nil {
		t.Fatalf("Update(status=closed, metadata): %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one bead.updated for a non-transitioning closed-row edit", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" || payload.Metadata["audit"] != "retained" {
		t.Fatalf("bead.updated payload = %+v ok=%v, want authoritative closed row with retained metadata", payload, ok)
	}
}

func TestCachingStorePrimedStaleOpenUpdateOnRemotelyClosedBeadEmitsBeadUpdated(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "stale cached open target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := base.Close(created.ID); err != nil {
		t.Fatalf("remote Close: %v", err)
	}

	closed := "closed"
	if err := cache.Update(created.ID, UpdateOpts{
		Status:   &closed,
		Metadata: map[string]string{"audit": "remote-close-retained"},
	}); err != nil {
		t.Fatalf("Update(status=closed, metadata): %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one bead.updated for the authoritative already-closed row", notes)
	}
}

func TestCachingStoreCapabilityLessStatusCloseDoesNotPreReadOrSynthesizeOnRefreshError(t *testing.T) {
	wantErr := errors.New("injected authoritative pre-read failure")
	base := NewMemStore()
	backing := &updatePreReadErrorStore{Store: base, getErr: wantErr}
	created, err := backing.Create(Bead{Title: "pre-read failure target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	closed := "closed"
	err = cache.Update(created.ID, UpdateOpts{Status: &closed})
	if err != nil {
		t.Fatalf("Update(status=closed): %v", err)
	}
	if backing.updateCalls != 1 {
		t.Fatalf("backing Update calls = %d, want one mutation without a classifier pre-read", backing.updateCalls)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none without an authoritative refresh payload", notes)
	}
	current, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get backing state: %v", err)
	}
	if current.Status != "closed" {
		t.Fatalf("backing status = %q, want committed close", current.Status)
	}
}

func TestCachingStoreCapabilityLessStatusCloseRefreshErrorDoesNotSynthesizeNotification(t *testing.T) {
	wantRefreshErr := errors.New("injected post-update refresh failure")
	backing := &updateRefreshErrorStore{Store: NewMemStore()}
	created, err := backing.Create(Bead{Title: "refresh-error close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backing.getErr = wantRefreshErr

	closed := "closed"
	if err := cache.Update(created.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update(status=closed): %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none without an authoritative refresh payload", notes)
	}
	if stats := cache.Stats(); !strings.Contains(stats.LastProblem, wantRefreshErr.Error()) {
		t.Fatalf("LastProblem = %q, want post-update refresh failure", stats.LastProblem)
	}
}

func TestCachingStoreCapabilityLessStatusClosePostCommitNotFoundDoesNotSynthesizeNotification(t *testing.T) {
	backing := &updatePostCommitNotFoundStore{Store: NewMemStore()}
	created, err := backing.Create(Bead{Title: "post-commit hidden close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	closed := "closed"
	if err := cache.Update(created.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update(status=closed): %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none for an authoritative post-commit miss", notes)
	}
}

func TestCachingStoreCapabilityLessStatusCloseEmitsBeadClosed(t *testing.T) {
	backing := capabilityStrippedUpdateStore{Store: NewMemStore()}
	created, err := backing.Create(Bead{Title: "capability-less close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	closed := "closed"
	if err := cache.Update(created.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("Update(status=closed): %v", err)
	}
	// A committed status close on a capability-less backing publishes the close
	// edge so eventexport and bead.closed order gates observe it.
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one bead.closed", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" {
		t.Fatalf("closed payload = %+v ok=%v, want authoritative closed row", payload, ok)
	}
}

func TestCachingStoreStatusCloseUsesBackingTransitionAcrossConcurrentReopenInterleaving(t *testing.T) {
	backing := &reopenDuringLegacyUpdateStore{MemStore: NewMemStore()}
	created, err := backing.Create(Bead{Title: "closed before peer reopen"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := backing.Close(created.ID); err != nil {
		t.Fatalf("Close setup: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	closed := "closed"
	title := "closed after peer reopen"
	if err := cache.Update(created.ID, UpdateOpts{
		Status:   &closed,
		Title:    &title,
		Labels:   []string{"transition-owned"},
		Metadata: map[string]string{"audit": "atomic-full-update"},
	}); err != nil {
		t.Fatalf("Update(status=closed): %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one backing-owned bead.closed", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" || payload.Title != title || payload.Metadata["audit"] != "atomic-full-update" {
		t.Fatalf("bead.closed payload = %#v ok=%v, want authoritative full update", payload, ok)
	}
	durable, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get durable row: %v", err)
	}
	if !payload.UpdatedAt.Equal(durable.UpdatedAt) {
		t.Fatalf("payload updated_at = %s, durable = %s", payload.UpdatedAt, durable.UpdatedAt)
	}
	cached, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get cached row: %v", err)
	}
	if cached.Revision != durable.Revision {
		t.Fatalf("cached revision = %d, durable = %d", cached.Revision, durable.Revision)
	}
}

func TestCachingStoreStatusClosePublishesCommittedTransitionBeforeReturningAncillaryError(t *testing.T) {
	wantErr := errors.New("commit acknowledgement failed")
	backing := &updateTransitionCommitErrorStore{MemStore: NewMemStore(), err: wantErr}
	created, err := backing.Create(Bead{Title: "commit-error close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	closed := "closed"
	err = cache.Update(created.ID, UpdateOpts{Status: &closed})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want %v", err, wantErr)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one proven bead.closed before returning the error", notes)
	}
	cached, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get cached row: %v", err)
	}
	durable, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get durable row: %v", err)
	}
	if cached.Status != "closed" || cached.Revision != durable.Revision || !cached.UpdatedAt.Equal(durable.UpdatedAt) {
		t.Fatalf("cached row = %#v, durable = %#v", cached, durable)
	}
}

func TestCachingStoreStatusClosePublishesConservativeUpdateForAmbiguousCommit(t *testing.T) {
	wantErr := errors.New("commit acknowledgement failed")
	backing := &updateTransitionCommitErrorStore{
		MemStore:       NewMemStore(),
		err:            wantErr,
		stripOwnership: true,
	}
	created, err := backing.Create(Bead{Title: "ambiguous status-close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	closed := "closed"
	err = cache.Update(created.ID, UpdateOpts{Status: &closed})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want %v", err, wantErr)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one conservative bead.updated for the changed authoritative snapshot", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" {
		t.Fatalf("bead.updated payload = %#v ok=%v, want authoritative closed snapshot", payload, ok)
	}
}

func TestCachingStoreTxMetadataOnAlreadyClosedBeadEmitsBeadUpdated(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "closed tx metadata target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Tx("closed metadata edit", func(tx Tx) error {
		return tx.SetMetadataBatch(created.ID, map[string]string{"audit": "tx-retained"})
	}); err != nil {
		t.Fatalf("Tx(SetMetadataBatch): %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one bead.updated for closed-row tx metadata", notes)
	}
}
