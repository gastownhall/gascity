package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type txCloseCommitThenErrorStore struct {
	Store
	closeErr error
}

type txCloseCommitThenError struct {
	Tx
	closeErr error
}

func (s *txCloseCommitThenErrorStore) Tx(_ string, fn func(Tx) error) error {
	return fn(txCloseCommitThenError{Tx: s.Store, closeErr: s.closeErr})
}

func (tx txCloseCommitThenError) Close(id string) error {
	if err := tx.Tx.Close(id); err != nil {
		return err
	}
	return tx.closeErr
}

func TestCachingStoreTxNonAtomicCommittedCloseErrorFencesCacheWithoutFabricatingEvent(t *testing.T) {
	wantErr := errors.New("transaction close acknowledgement lost")
	base := NewMemStore()
	backing := &txCloseCommitThenErrorStore{Store: base, closeErr: wantErr}

	blocker, err := base.Create(Bead{Title: "transaction close blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	blockedProjection := true
	dependent, err := base.Create(Bead{
		Title:     "dependent with stale ready projection",
		Needs:     []string{blocker.ID},
		IsBlocked: &blockedProjection,
	})
	if err != nil {
		t.Fatalf("Create dependent: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	cache.mu.RLock()
	seqBefore := cache.beadSeq[blocker.ID]
	cache.mu.RUnlock()

	err = cache.Tx("ambiguous non-atomic close", func(tx Tx) error {
		return tx.Close(blocker.ID)
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Tx error = %v, want %v", err, wantErr)
	}

	durable, err := base.Get(blocker.ID)
	if err != nil {
		t.Fatalf("Get durable blocker: %v", err)
	}
	if durable.Status != "closed" {
		t.Fatalf("durable blocker status = %q, want closed", durable.Status)
	}

	cache.mu.RLock()
	_, dirty := cache.dirty[blocker.ID]
	seqAfter := cache.beadSeq[blocker.ID]
	cachedDependent := cloneBead(cache.beads[dependent.ID])
	cache.mu.RUnlock()
	if !dirty {
		t.Error("ambiguously committed close target is not dirty; a cache Get can return the stale open row")
	}
	if seqAfter <= seqBefore {
		t.Errorf("close target fence sequence = %d, want greater than pre-transaction %d", seqAfter, seqBefore)
	}
	if cachedDependent.IsBlocked != nil {
		t.Errorf("dependent cached IsBlocked = %v, want nil after ambiguous blocker status mutation", *cachedDependent.IsBlocked)
	}
	if len(notes) != 0 {
		t.Errorf("notifications = %+v, want none without an authoritative committed snapshot", notes)
	}

	converged, err := cache.Get(blocker.ID)
	if err != nil {
		t.Fatalf("Get blocker after ambiguous commit: %v", err)
	}
	if converged.Status != "closed" || converged.Revision != durable.Revision || !converged.UpdatedAt.Equal(durable.UpdatedAt) {
		t.Errorf("converged blocker = %#v, want durable blocker %#v", converged, durable)
	}
	cache.mu.RLock()
	_, stillDirty := cache.dirty[blocker.ID]
	cache.mu.RUnlock()
	if stillDirty {
		t.Error("close target remained dirty after an authoritative Get converged the cache")
	}
}

func TestCachingStoreTxCreateEmitsBeadCreatedWithCompletePayload(t *testing.T) {
	base := NewMemStore()
	dependency, err := base.Create(Bead{Title: "transaction create dependency"})
	if err != nil {
		t.Fatalf("Create dependency: %v", err)
	}

	var notes []cacheWriteNotification
	cache := NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		notes = append(notes, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	priority := 2
	deferUntil := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var created Bead
	err = cache.Tx("create dependent bead", func(tx Tx) error {
		var createErr error
		created, createErr = tx.Create(Bead{
			Title:       "transaction-created child",
			Type:        "feature",
			Priority:    &priority,
			Assignee:    "worker/one",
			From:        "controller",
			ParentID:    "gc-parent",
			Ref:         "build-step",
			Needs:       []string{dependency.ID},
			Description: "created inside a backing transaction",
			Labels:      []string{"tx-created", "projection-input"},
			Metadata:    map[string]string{"trace": "complete-payload"},
			Ephemeral:   true,
			NoHistory:   true,
			DeferUntil:  &deferUntil,
		})
		return createErr
	})
	if err != nil {
		t.Fatalf("Tx(Create): %v", err)
	}

	if len(notes) != 1 {
		t.Fatalf("notifications = %+v, want one bead.created", notes)
	}
	if notes[0].eventType != "bead.created" || notes[0].beadID != created.ID {
		t.Fatalf("notification = %+v, want bead.created for %s", notes[0], created.ID)
	}
	payload, ok := DecodeBeadEventPayload(notes[0].payload)
	if !ok {
		t.Fatalf("bead.created payload did not decode: %s", notes[0].payload)
	}
	if payload.ID != created.ID || payload.Title != "transaction-created child" || payload.Status != "open" ||
		payload.Type != "feature" || payload.Priority == nil || *payload.Priority != priority ||
		payload.Assignee != "worker/one" || payload.From != "controller" || payload.ParentID != "gc-parent" ||
		payload.Ref != "build-step" || payload.Description != "created inside a backing transaction" ||
		len(payload.Labels) != 2 || payload.Labels[0] != "tx-created" || payload.Labels[1] != "projection-input" ||
		payload.Metadata["trace"] != "complete-payload" || !payload.Ephemeral || !payload.NoHistory ||
		payload.DeferUntil == nil || !payload.DeferUntil.Equal(deferUntil) || payload.CreatedAt.IsZero() || payload.UpdatedAt.IsZero() {
		t.Fatalf("bead.created payload = %#v, want complete created snapshot", payload)
	}
	if len(payload.Dependencies) != 1 || payload.Dependencies[0].IssueID != created.ID ||
		payload.Dependencies[0].DependsOnID != dependency.ID || payload.Dependencies[0].Type != "blocks" {
		t.Fatalf("bead.created dependencies = %#v, want full dependency on %s", payload.Dependencies, dependency.ID)
	}
}
