package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

type typedBlockedCloseStore struct {
	*beads.MemStore
	combinedErr  error
	closeErr     error
	atomic       bool
	metadataOnly int
	forceClose   int
}

func (s *typedBlockedCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" {
		return s.combinedErr
	}
	s.metadataOnly++
	return s.MemStore.Update(id, opts)
}

func (s *typedBlockedCloseStore) Close(id string) error {
	s.forceClose++
	return s.MemStore.Close(id)
}

func (s *typedBlockedCloseStore) AtomicTx() bool { return s.atomic }

func (s *typedBlockedCloseStore) Tx(_ string, fn func(beads.Tx) error) error {
	tx := &typedBlockedCloseTx{store: s}
	if err := fn(tx); err != nil {
		return err
	}
	for _, update := range tx.updates {
		if err := s.MemStore.Update(update.id, update.opts); err != nil {
			return err
		}
	}
	if tx.closeID != "" {
		return s.MemStore.Close(tx.closeID)
	}
	return nil
}

type typedBlockedCloseUpdate struct {
	id   string
	opts beads.UpdateOpts
}

type typedBlockedCloseTx struct {
	store   *typedBlockedCloseStore
	updates []typedBlockedCloseUpdate
	closeID string
}

func (tx *typedBlockedCloseTx) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("unexpected Create in close compatibility transaction")
}

func (tx *typedBlockedCloseTx) Update(id string, opts beads.UpdateOpts) error {
	tx.store.metadataOnly++
	tx.updates = append(tx.updates, typedBlockedCloseUpdate{id: id, opts: opts})
	return nil
}

func (tx *typedBlockedCloseTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return tx.Update(id, beads.UpdateOpts{Metadata: kvs})
}

func (tx *typedBlockedCloseTx) Close(id string) error {
	tx.store.forceClose++
	if tx.store.closeErr != nil {
		return tx.store.closeErr
	}
	tx.closeID = id
	return nil
}

func TestUpdateMetadataAndCloseFallsBackAtomicallyForTypedClosePolicy(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{beads.ErrCloseBlocked, beads.ErrCloseOpenChildren} {
		mem := beads.NewMemStore()
		body := mustCreate(t, mem, beads.Bead{Title: "scope body", Status: "open"})
		store := &typedBlockedCloseStore{
			MemStore:    mem,
			combinedErr: fmt.Errorf("update refused: %w", refusal),
			atomic:      true,
		}

		if err := updateMetadataAndClose(store, body.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}); err != nil {
			t.Fatalf("updateMetadataAndClose(%v): %v", refusal, err)
		}
		got, err := store.Get(body.ID)
		if err != nil {
			t.Fatalf("get body: %v", err)
		}
		if got.Status != "closed" {
			t.Fatalf("status = %q, want closed", got.Status)
		}
		if got.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
			t.Fatalf("outcome metadata = %q, want %q", got.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomePass)
		}
		if store.metadataOnly != 1 || store.forceClose != 1 {
			t.Fatalf("metadata-only updates=%d force closes=%d, want 1 each", store.metadataOnly, store.forceClose)
		}
	}
}

func TestUpdateMetadataAndCloseNeverForcesAfterGenericFailure(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	body := mustCreate(t, mem, beads.Bead{Title: "scope body", Status: "open"})
	wantErr := errors.New("transport failed")
	store := &typedBlockedCloseStore{MemStore: mem, combinedErr: wantErr, atomic: true}

	err := updateMetadataAndClose(store, body.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want transport failure", err)
	}
	if store.metadataOnly != 0 || store.forceClose != 0 {
		t.Fatalf("generic failure triggered metadata-only=%d force-close=%d, want zero", store.metadataOnly, store.forceClose)
	}
	got, getErr := store.Get(body.ID)
	if getErr != nil {
		t.Fatalf("get body: %v", getErr)
	}
	if got.Status != "open" || len(got.Metadata) != 0 {
		t.Fatalf("body mutated after generic failure: %+v", got)
	}
}

func TestUpdateMetadataAndCloseRollsBackMetadataWhenForcedCloseFails(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	body := mustCreate(t, mem, beads.Bead{Title: "scope body", Status: "open", Metadata: map[string]string{"kept": "yes"}})
	wantErr := errors.New("injected close failure")
	store := &typedBlockedCloseStore{
		MemStore:    mem,
		combinedErr: fmt.Errorf("update refused: %w", beads.ErrCloseOpenChildren),
		closeErr:    wantErr,
		atomic:      true,
	}

	err := updateMetadataAndClose(store, body.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	got, getErr := store.Get(body.ID)
	if getErr != nil {
		t.Fatalf("get body: %v", getErr)
	}
	if got.Status != "open" || got.Metadata["kept"] != "yes" || got.Metadata[beadmeta.OutcomeMetadataKey] != "" {
		t.Fatalf("failed atomic fallback left partial state: %+v", got)
	}
}

func TestUpdateMetadataAndCloseFailsClosedWithoutAtomicFallback(t *testing.T) {
	t.Parallel()
	mem := beads.NewMemStore()
	body := mustCreate(t, mem, beads.Bead{Title: "scope body", Status: "open"})
	store := &typedBlockedCloseStore{
		MemStore:    mem,
		combinedErr: fmt.Errorf("update refused: %w", beads.ErrCloseOpenChildren),
	}

	err := updateMetadataAndClose(store, body.ID, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass})
	if !errors.Is(err, beads.ErrCloseOpenChildren) {
		t.Fatalf("error = %v, want original typed refusal", err)
	}
	if store.metadataOnly != 0 || store.forceClose != 0 {
		t.Fatalf("non-atomic fallback attempted writes: metadata=%d close=%d", store.metadataOnly, store.forceClose)
	}
}
