package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func TestBeadPolicyStoreForwardsCloseTransitionWhenConditionalWritesAreOff(t *testing.T) {
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Off,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("OpenStoreAtForCity: %v", err)
	}

	created, err := result.Store.Create(beads.Bead{Title: "policy close target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(result.Store, nil)
	closer, ok := beads.CloseTransitionerFor(wrapped)
	if !ok {
		t.Fatalf("CloseTransitionerFor(%T) reported unsupported", wrapped)
	}

	transition, err := closer.CloseWithReasonIfOpen(created.ID, "policy atomic close")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned {
		t.Fatal("Transitioned = false, want true")
	}
	if transition.After.Metadata["close_reason"] != "policy atomic close" {
		t.Fatalf("close_reason = %q, want policy atomic close", transition.After.Metadata["close_reason"])
	}
}

func TestBeadPolicyStoreForwardsUpdateTransitioner(t *testing.T) {
	base := beads.NewMemStore()
	wrapped := wrapStoreWithBeadPolicies(base, nil)

	transitioner, ok := beads.UpdateTransitionerFor(wrapped)
	if !ok {
		t.Fatalf("UpdateTransitionerFor(%T) reported unsupported", wrapped)
	}
	if transitioner != beads.UpdateTransitioner(base) {
		t.Fatalf("UpdateTransitionerFor(%T) returned %T, want exact underlying %T", wrapped, transitioner, base)
	}
}

func TestBeadPolicyStoreForwardsCloseAllTransitioner(t *testing.T) {
	base := beads.NewMemStore()
	wrapped := wrapStoreWithBeadPolicies(base, nil)

	transitioner, ok := beads.CloseAllTransitionerFor(wrapped)
	if !ok {
		t.Fatalf("CloseAllTransitionerFor(%T) reported unsupported", wrapped)
	}
	if transitioner != beads.CloseAllTransitioner(base) {
		t.Fatalf("CloseAllTransitionerFor(%T) returned %T, want exact underlying %T", wrapped, transitioner, base)
	}
}

func TestBeadPolicyStoreForwardsObserverBarrier(t *testing.T) {
	cache := beads.NewCachingStoreForTest(beads.NewMemStore(), nil)
	wrapped := wrapStoreWithBeadPolicies(cache, nil)

	barrier, ok := beads.ObserverBarrierFor(wrapped)
	if !ok {
		t.Fatalf("ObserverBarrierFor(%T) reported unsupported", wrapped)
	}
	if barrier != beads.ObserverBarrier(cache) {
		t.Fatalf("ObserverBarrierFor(%T) returned %T, want exact underlying %T", wrapped, barrier, cache)
	}
}

func TestBeadPolicyStoreForwardsLifecycleMetadataTransaction(t *testing.T) {
	backing := &policyLifecycleMetadataStore{MemStore: beads.NewMemStore()}
	created, err := backing.Create(beads.Bead{Title: "policy lifecycle target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wrapped := wrapStoreWithBeadPolicies(backing, nil)

	err = beads.WithLifecycleMetadataTransaction(wrapped, created.ID, func(tx beads.LifecycleMetadataTransaction) error {
		return tx.SetMetadata("lifecycle", "forwarded")
	})
	if err != nil {
		t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
	}
	if backing.calls != 1 {
		t.Fatalf("backing lifecycle transaction calls = %d, want 1", backing.calls)
	}
	after, err := backing.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := after.Metadata["lifecycle"]; got != "forwarded" {
		t.Fatalf("lifecycle metadata = %q, want forwarded", got)
	}
}

type policyLifecycleMetadataStore struct {
	*beads.MemStore
	calls int
}

func (s *policyLifecycleMetadataStore) WithLifecycleMetadataTransaction(id string, fn func(beads.LifecycleMetadataTransaction) error) error {
	s.calls++
	return fn(policyLifecycleMetadataTransaction{store: s.MemStore, id: id})
}

type policyLifecycleMetadataTransaction struct {
	store *beads.MemStore
	id    string
}

func (tx policyLifecycleMetadataTransaction) Get() (beads.Bead, error) {
	return tx.store.Get(tx.id)
}

func (tx policyLifecycleMetadataTransaction) SetMetadata(key, value string) error {
	return tx.store.SetMetadata(tx.id, key, value)
}

func (tx policyLifecycleMetadataTransaction) SetMetadataBatch(values map[string]string) error {
	return tx.store.SetMetadataBatch(tx.id, values)
}
