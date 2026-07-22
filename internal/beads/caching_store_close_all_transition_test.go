package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type closeAllCapabilityStrippedStore struct{ Store }

type closeAllPartialCommitErrorStore struct {
	*MemStore
	err error
}

type closeAllDuplicateCountStore struct{ *MemStore }

type closeAllFixedTransitionStore struct {
	Store
	result CloseAllTransitionResult
	err    error
}

func (s closeAllFixedTransitionStore) CloseAllWithTransitions([]string, map[string]string) (CloseAllTransitionResult, error) {
	return s.result, s.err
}

func (s *closeAllPartialCommitErrorStore) CloseAllWithTransitions(ids []string, metadata map[string]string) (CloseAllTransitionResult, error) {
	if len(ids) == 0 {
		return CloseAllTransitionResult{}, s.err
	}
	result, err := s.MemStore.CloseAllWithTransitions(ids[:1], metadata)
	return result, errors.Join(err, s.err)
}

func (s *closeAllDuplicateCountStore) CloseAllWithTransitions(ids []string, metadata map[string]string) (CloseAllTransitionResult, error) {
	result, err := s.MemStore.CloseAllWithTransitions(ids, metadata)
	if len(ids) > 1 {
		result.Count = len(ids)
	}
	return result, err
}

func TestCachingStoreCloseAllUsesBackingTransitionWhenCachedStateIsStaleClosed(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "batch blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	created, err := base.Create(Bead{Title: "batch payload", Labels: []string{"payload-label"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Close(created.ID); err != nil {
		t.Fatalf("cache Close setup: %v", err)
	}
	notes = nil
	if err := base.Reopen(created.ID); err != nil {
		t.Fatalf("remote Reopen: %v", err)
	}

	closed, err := cache.CloseAll([]string{created.ID}, map[string]string{"batch": "authoritative"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 1 {
		t.Fatalf("CloseAll count = %d, want 1", closed)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one backing-owned bead.closed", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.ID != created.ID || payload.Status != "closed" || payload.Title != "batch payload" ||
		payload.Metadata["batch"] != "authoritative" || len(payload.Labels) != 1 || payload.Labels[0] != "payload-label" ||
		len(payload.Dependencies) != 1 || payload.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("bead.closed payload = %#v ok=%v, want complete authoritative batch snapshot", payload, ok)
	}
}

func TestCachingStoreCloseAllDoesNotInferTransitionFromStaleOpenCache(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "already closed remotely"})
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

	closed, err := cache.CloseAll([]string{created.ID}, map[string]string{"batch": "ignored-for-closed"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 0 {
		t.Fatalf("CloseAll count = %d, want 0 for already-closed bead", closed)
	}
	if len(notes) != 0 {
		t.Fatalf("notifications = %+v, want none for backing-observed already-closed bead", notes)
	}
	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get cached authoritative result: %v", err)
	}
	if got.Status != "closed" || got.Metadata["batch"] != "" {
		t.Fatalf("cached bead = %#v, want unchanged authoritative closed snapshot", got)
	}
}

func TestCachingStoreCloseAllPublishesAuthoritativeMetadataOnlyObservation(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{
		Title:    "metadata-only batch result",
		Metadata: StringMap{"batch": "before"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before, err := base.Get(created.ID)
	if err != nil {
		t.Fatalf("Get closed bead: %v", err)
	}
	after := cloneBead(before)
	after.Metadata["batch"] = "after"
	backing := closeAllFixedTransitionStore{
		Store: base,
		result: CloseAllTransitionResult{
			Transitions: map[string]CloseTransition{
				before.ID: {Before: before, After: after},
			},
		},
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	if _, err := cache.CloseAll([]string{before.ID}, map[string]string{"batch": "after"}); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.updated" || notes[0].beadID != before.ID {
		t.Fatalf("notifications = %+v, want one authoritative bead.updated", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" || payload.Metadata["batch"] != "after" {
		t.Fatalf("updated payload = %#v ok=%v, want exact metadata-only result", payload, ok)
	}
}

func TestCachingStoreCloseAllPublishesCommittedTransitionsReturnedWithPartialError(t *testing.T) {
	wantErr := errors.New("injected second close failure")
	backing := &closeAllPartialCommitErrorStore{MemStore: NewMemStore(), err: wantErr}
	first, err := backing.Create(Bead{Title: "committed first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := backing.Create(Bead{Title: "unprocessed second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{first.ID, second.ID}, map[string]string{"batch": "partial"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("CloseAll error = %v, want %v", err, wantErr)
	}
	if closed != 1 {
		t.Fatalf("CloseAll count = %d, want exact backing count 1", closed)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != first.ID {
		t.Fatalf("notifications = %+v, want only the proven committed transition", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" || payload.Metadata["batch"] != "partial" {
		t.Fatalf("committed payload = %#v ok=%v, want authoritative metadata", payload, ok)
	}

	cache.mu.RLock()
	_, secondDirty := cache.dirty[second.ID]
	cache.mu.RUnlock()
	if !secondDirty {
		t.Fatal("unprocessed second bead was not marked dirty after partial error")
	}
}

func TestCachingStoreLegacyCloseAllPublishesAuthoritativeClosedObservation(t *testing.T) {
	base := NewMemStore()
	blocker, err := base.Create(Bead{Title: "legacy batch blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	created, err := base.Create(Bead{Title: "legacy batch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := base.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	backing := closeAllCapabilityStrippedStore{Store: base}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	closed, err := cache.CloseAll([]string{created.ID}, map[string]string{"batch": "legacy"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 1 {
		t.Fatalf("CloseAll count = %d, want 1", closed)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one authoritative bead.closed observation", notes)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok || payload.Status != "closed" || payload.Metadata["batch"] != "legacy" ||
		len(payload.Dependencies) != 1 || payload.Dependencies[0].DependsOnID != blocker.ID {
		t.Fatalf("closed payload = %#v ok=%v, want authoritative closed legacy row", payload, ok)
	}
	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get refreshed closed bead: %v", err)
	}
	if got.Status != "closed" || got.Metadata["batch"] != "legacy" {
		t.Fatalf("cached bead = %#v, want refreshed authoritative legacy result", got)
	}
}

func TestCachingStoreCloseAllPreservesDuplicateCountWithoutDuplicateNotification(t *testing.T) {
	backing := &closeAllDuplicateCountStore{MemStore: NewMemStore()}
	created, err := backing.Create(Bead{Title: "duplicate batch ID"})
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

	closed, err := cache.CloseAll([]string{created.ID, created.ID}, map[string]string{"batch": "duplicate"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseAll count = %d, want exact backing-reported duplicate count 2", closed)
	}
	if len(notes) != 1 || notes[0].eventType != "bead.closed" || notes[0].beadID != created.ID {
		t.Fatalf("notifications = %+v, want one per-ID transition notification", notes)
	}
}
