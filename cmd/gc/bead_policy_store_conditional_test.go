package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// atomicCloseCapableStore is a wiring-only fake. Its two conditional writes
// are not the atomicity proof; internal/beads native tests own that contract.
type atomicCloseCapableStore struct{ beads.Store }

func (s *atomicCloseCapableStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	writer, ok := beads.ConditionalWriterFor(s.Store)
	if !ok {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	if err := writer.UpdateIfMatch(id, expectedRevision, beads.UpdateOpts{Metadata: metadata}); err != nil {
		return beads.Bead{}, err
	}
	updated, err := s.Get(id)
	if err != nil {
		return beads.Bead{}, err
	}
	if err := writer.CloseIfMatch(id, updated.Revision); err != nil {
		return beads.Bead{}, err
	}
	return s.Get(id)
}

// TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper pins the stage-3
// wiring hazard: every factory store is policy-wrapped, and interface
// embedding hides the factory's conditional-writes stamp — without the
// wrapper's declared resolution target, a require deployment would silently
// resolve unset→legacy through the wrapper on every consumer.
func TestBeadPolicyStoreResolvesConditionalWritesThroughWrapper(t *testing.T) {
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}

	wrapped := wrapStoreWithBeadPolicies(result.Store, nil)
	if _, _, ok := unwrapBeadPolicyStore(wrapped); !ok {
		t.Fatalf("test premise: store %T is not policy-wrapped", wrapped)
	}

	writer, diag, resolveErr := beads.ResolveConditionalWriter(wrapped)
	if resolveErr != nil || diag != nil {
		t.Fatalf("resolve through policy wrapper = diag %v err %v, want the stamped store's writer", diag, resolveErr)
	}
	if writer == nil {
		t.Fatal("resolve through policy wrapper returned no writer: the require stamp was hidden by interface embedding")
	}
}

func TestBeadPolicyStoreResolvesAtomicCloseThroughProductionWrapperOrder(t *testing.T) {
	backing := &atomicCloseCapableStore{Store: beads.NewMemStore()}
	var notifications []string
	cache := beads.NewCachingStoreForTest(backing, func(eventType, _ string, _ json.RawMessage) {
		notifications = append(notifications, eventType)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime cache: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(cache, nil)
	created, err := wrapped.Create(beads.Bead{Title: "production wrapper atomic close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	notifications = nil

	closer, ok := beads.AtomicConditionalCloserFor(wrapped)
	if !ok {
		t.Fatal("policy(cache(capable)) did not expose atomic close")
	}
	closed, err := closer.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
		t.Fatalf("closed bead = %#v, want terminal row", closed)
	}
	if len(notifications) != 1 || notifications[0] != "bead.closed" {
		t.Fatalf("notifications = %v, want cache-preserved bead.closed", notifications)
	}

	unsupported := wrapStoreWithBeadPolicies(beads.NewCachingStoreForTest(beads.NewMemStore(), nil), nil)
	if closer, ok := beads.AtomicConditionalCloserFor(unsupported); ok || closer != nil {
		t.Fatalf("policy(cache(unsupported)) capability = (%T, %v), want unavailable", closer, ok)
	}
}
