package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type commitThenErrorCacheWriteStore struct {
	Store
	writeErr error
	getCalls int
}

func (s *commitThenErrorCacheWriteStore) Get(id string) (Bead, error) {
	s.getCalls++
	return s.Store.Get(id)
}

func (s *commitThenErrorCacheWriteStore) Reopen(id string) error {
	if err := s.Store.Reopen(id); err != nil {
		return err
	}
	return s.writeErr
}

func (s *commitThenErrorCacheWriteStore) SetMetadata(id, key, value string) error {
	if err := s.Store.SetMetadata(id, key, value); err != nil {
		return err
	}
	return s.writeErr
}

func (s *commitThenErrorCacheWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := s.Store.SetMetadataBatch(id, kvs); err != nil {
		return err
	}
	return s.writeErr
}

func assertAmbiguousCacheWriteFenced(t *testing.T, cache *CachingStore, id string, seqBefore uint64) {
	t.Helper()

	cache.mu.RLock()
	seqAfter := cache.beadSeq[id]
	_, dirty := cache.dirty[id]
	cache.mu.RUnlock()

	if seqAfter <= seqBefore {
		t.Errorf("beadSeq after ambiguous write = %d, want newer than %d", seqAfter, seqBefore)
	}
	if !dirty {
		t.Error("ambiguous write left the cached row clean")
	}
}

func cacheMutationSeqForTest(cache *CachingStore) uint64 {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.mutationSeq
}

func TestCachingStoreReopenAmbiguousCommitErrorFencesUntilBackingConverges(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "ambiguous reopen"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantErr := errors.New("connection reset after reopen commit")
	backing := &commitThenErrorCacheWriteStore{Store: base, writeErr: wantErr}
	observations := 0
	cache := NewCachingStoreForTest(backing, func(_, _ string, _ json.RawMessage) {
		observations++
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := cache.Close(created.ID); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
	observations = 0
	backing.getCalls = 0
	seqBefore := cacheMutationSeqForTest(cache)

	if err := cache.Reopen(created.ID); !errors.Is(err, wantErr) {
		t.Fatalf("Reopen error = %v, want %v", err, wantErr)
	}

	assertAmbiguousCacheWriteFenced(t, cache, created.ID, seqBefore)
	if observations != 0 {
		t.Errorf("observer notifications after ambiguous Reopen = %d, want 0", observations)
	}

	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after ambiguous Reopen: %v", err)
	}
	if backing.getCalls != 1 {
		t.Errorf("backing Get calls after ambiguous Reopen = %d, want 1", backing.getCalls)
	}
	if got.Status != "open" {
		t.Errorf("status after ambiguous Reopen convergence = %q, want open", got.Status)
	}
	if observations != 0 {
		t.Errorf("observer notifications after convergence Get = %d, want 0", observations)
	}
}

func TestCachingStoreSetMetadataAmbiguousCommitErrorFencesUntilBackingConverges(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "ambiguous metadata"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantErr := errors.New("connection reset after metadata commit")
	backing := &commitThenErrorCacheWriteStore{Store: base, writeErr: wantErr}
	observations := 0
	cache := NewCachingStoreForTest(backing, func(_, _ string, _ json.RawMessage) {
		observations++
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	seqBefore := cacheMutationSeqForTest(cache)

	if err := cache.SetMetadata(created.ID, "phase", "committed"); !errors.Is(err, wantErr) {
		t.Fatalf("SetMetadata error = %v, want %v", err, wantErr)
	}

	assertAmbiguousCacheWriteFenced(t, cache, created.ID, seqBefore)
	if observations != 0 {
		t.Errorf("observer notifications after ambiguous SetMetadata = %d, want 0", observations)
	}

	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after ambiguous SetMetadata: %v", err)
	}
	if backing.getCalls != 1 {
		t.Errorf("backing Get calls after ambiguous SetMetadata = %d, want 1", backing.getCalls)
	}
	if got.Metadata["phase"] != "committed" {
		t.Errorf("metadata after ambiguous SetMetadata convergence = %q, want committed", got.Metadata["phase"])
	}
	if observations != 0 {
		t.Errorf("observer notifications after convergence Get = %d, want 0", observations)
	}
}

func TestCachingStoreSetMetadataBatchAmbiguousCommitErrorFencesUntilBackingConverges(t *testing.T) {
	base := NewMemStore()
	created, err := base.Create(Bead{Title: "ambiguous metadata batch"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantErr := errors.New("connection reset after metadata batch commit")
	backing := &commitThenErrorCacheWriteStore{Store: base, writeErr: wantErr}
	observations := 0
	cache := NewCachingStoreForTest(backing, func(_, _ string, _ json.RawMessage) {
		observations++
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	seqBefore := cacheMutationSeqForTest(cache)

	wantMetadata := map[string]string{"phase": "committed", "owner": "remote"}
	if err := cache.SetMetadataBatch(created.ID, wantMetadata); !errors.Is(err, wantErr) {
		t.Fatalf("SetMetadataBatch error = %v, want %v", err, wantErr)
	}

	assertAmbiguousCacheWriteFenced(t, cache, created.ID, seqBefore)
	if observations != 0 {
		t.Errorf("observer notifications after ambiguous SetMetadataBatch = %d, want 0", observations)
	}

	got, err := cache.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after ambiguous SetMetadataBatch: %v", err)
	}
	if backing.getCalls != 1 {
		t.Errorf("backing Get calls after ambiguous SetMetadataBatch = %d, want 1", backing.getCalls)
	}
	for key, want := range wantMetadata {
		if got.Metadata[key] != want {
			t.Errorf("metadata[%q] after ambiguous SetMetadataBatch convergence = %q, want %q", key, got.Metadata[key], want)
		}
	}
	if observations != 0 {
		t.Errorf("observer notifications after convergence Get = %d, want 0", observations)
	}
}
