package beads

import (
	"errors"
	"testing"
)

func TestApplyMetadataCASOutcomes(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	bead, err := store.Create(Bead{
		Title:    "review receipt",
		Metadata: StringMap{"review_sha": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := ApplyMetadataCAS(store, bead.ID, "review_sha", "old", "new")
	if err != nil {
		t.Fatalf("winning CAS: %v", err)
	}
	if outcome != MetadataCASSwapped {
		t.Fatalf("winning outcome = %q, want %q", outcome, MetadataCASSwapped)
	}

	outcome, err = ApplyMetadataCAS(store, bead.ID, "review_sha", "old", "new")
	if err != nil {
		t.Fatalf("replayed CAS: %v", err)
	}
	if outcome != MetadataCASAlreadyNext {
		t.Fatalf("replayed outcome = %q, want %q", outcome, MetadataCASAlreadyNext)
	}

	outcome, err = ApplyMetadataCAS(store, bead.ID, "review_sha", "old", "other")
	if err != nil {
		t.Fatalf("conflicting CAS: %v", err)
	}
	if outcome != MetadataCASConflict {
		t.Fatalf("conflicting outcome = %q, want %q", outcome, MetadataCASConflict)
	}
}

func TestApplyMetadataCASExpectedEqualsNextIsSwapped(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	bead, err := store.Create(Bead{
		Title:    "idempotent review receipt",
		Metadata: StringMap{"review_sha": "same"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	outcome, err := ApplyMetadataCAS(store, bead.ID, "review_sha", "same", "same")
	if err != nil {
		t.Fatalf("ApplyMetadataCAS: %v", err)
	}
	if outcome != MetadataCASSwapped {
		t.Fatalf("outcome = %q, want %q when expected == next and the CAS succeeds", outcome, MetadataCASSwapped)
	}
}

func TestApplyMetadataCASRequiresCapability(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "review receipt",
		Metadata: StringMap{"review_sha": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := storeWithoutMetadataCAS{Store: backing}
	_, err = ApplyMetadataCAS(store, bead.ID, "review_sha", "old", "new")
	if !errors.Is(err, ErrConditionalWriteUnsupported) {
		t.Fatalf("ApplyMetadataCAS error = %v, want ErrConditionalWriteUnsupported", err)
	}
	got, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["review_sha"] != "old" {
		t.Fatalf("metadata changed through an unconditional fallback: %v", got.Metadata)
	}
}

func TestApplyMetadataCASTransportErrorIsNotReclassified(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ambiguous commit")
	store := &metadataCASTransportErrorStore{
		Store:  NewMemStore(),
		casErr: sentinel,
	}
	_, err := ApplyMetadataCAS(store, "gc-1", "review_sha", "old", "new")
	if !errors.Is(err, sentinel) {
		t.Fatalf("ApplyMetadataCAS error = %v, want %v", err, sentinel)
	}
	if store.getCalls != 0 {
		t.Fatalf("Get calls = %d, want 0 after CAS transport error", store.getCalls)
	}
}

func TestApplyMetadataCASRetryAfterAmbiguousCommitIsAlreadyNext(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{
		Title:    "review receipt",
		Metadata: StringMap{"review_sha": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &metadataCASAmbiguousCommitStore{
		Store: backing,
	}

	if _, err := ApplyMetadataCAS(store, bead.ID, "review_sha", "old", "new"); err == nil {
		t.Fatal("first ApplyMetadataCAS error=nil, want ambiguous commit error")
	}
	outcome, err := ApplyMetadataCAS(store, bead.ID, "review_sha", "old", "new")
	if err != nil {
		t.Fatalf("retry ApplyMetadataCAS: %v", err)
	}
	if outcome != MetadataCASAlreadyNext {
		t.Fatalf("retry outcome = %q, want %q", outcome, MetadataCASAlreadyNext)
	}
}

func TestApplyMetadataCASFalseResultRequiresAuthoritativeReadback(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("readback failed")
	store := &metadataCASReadbackErrorStore{
		Store:  NewMemStore(),
		getErr: sentinel,
	}
	_, err := ApplyMetadataCAS(store, "gc-1", "review_sha", "old", "new")
	if !errors.Is(err, sentinel) {
		t.Fatalf("ApplyMetadataCAS error = %v, want %v", err, sentinel)
	}
}

type storeWithoutMetadataCAS struct {
	Store
}

type metadataCASTransportErrorStore struct {
	Store
	casErr   error
	getCalls int
}

func (s *metadataCASTransportErrorStore) CompareAndSetMetadataKey(_, _, _, _ string) (bool, error) {
	return false, s.casErr
}

func (s *metadataCASTransportErrorStore) Get(id string) (Bead, error) {
	s.getCalls++
	return s.Store.Get(id)
}

type metadataCASReadbackErrorStore struct {
	Store
	getErr error
}

func (s *metadataCASReadbackErrorStore) CompareAndSetMetadataKey(_, _, _, _ string) (bool, error) {
	return false, nil
}

func (s *metadataCASReadbackErrorStore) Get(_ string) (Bead, error) {
	return Bead{}, s.getErr
}

type metadataCASAmbiguousCommitStore struct {
	Store
	first bool
}

func (s *metadataCASAmbiguousCommitStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	writer, ok := MetadataCASWriterFor(s.Store)
	if !ok {
		return false, ErrConditionalWriteUnsupported
	}
	swapped, err := writer.CompareAndSetMetadataKey(id, key, expected, next)
	if err != nil {
		return false, err
	}
	if !s.first {
		s.first = true
		return false, errors.New("connection lost after commit")
	}
	return swapped, nil
}
