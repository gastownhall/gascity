package beads

import (
	"errors"
	"strings"
	"testing"
)

func TestApplyUpdateCASUpdatesAndReplaysWithoutAnotherWrite(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	bead, err := store.Create(Bead{
		Title:       "old title",
		Description: "old description",
		Metadata:    StringMap{"github.projection_hash": "old"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	priority := 1
	title := "new title"
	description := "new description"
	status := "in_progress"
	issueType := "feature"
	opts := UpdateOpts{
		Title:       &title,
		Description: &description,
		Status:      &status,
		Type:        &issueType,
		Priority:    &priority,
		Metadata:    map[string]string{"github.projection_hash": "new"},
	}

	result, err := ApplyUpdateCAS(store, bead.ID, bead.Revision, opts)
	if err != nil {
		t.Fatalf("ApplyUpdateCAS: %v", err)
	}
	if result.Outcome != UpdateCASUpdated {
		t.Fatalf("outcome = %q, want %q", result.Outcome, UpdateCASUpdated)
	}
	if result.PreviousRevision != bead.Revision {
		t.Fatalf("previous revision = %d, want %d", result.PreviousRevision, bead.Revision)
	}
	if result.Revision == bead.Revision {
		t.Fatalf("revision did not move after update: %d", result.Revision)
	}

	replay, err := ApplyUpdateCAS(store, bead.ID, bead.Revision, opts)
	if err != nil {
		t.Fatalf("replay ApplyUpdateCAS: %v", err)
	}
	if replay.Outcome != UpdateCASAlreadyApplied {
		t.Fatalf("replay outcome = %q, want %q", replay.Outcome, UpdateCASAlreadyApplied)
	}
	if replay.Revision != result.Revision {
		t.Fatalf("replay revision = %d, want unchanged %d", replay.Revision, result.Revision)
	}
}

func TestApplyUpdateCASConflictDoesNotWrite(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	bead, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	concurrent := "concurrent"
	if err := store.Update(bead.ID, UpdateOpts{Title: &concurrent}); err != nil {
		t.Fatalf("concurrent Update: %v", err)
	}
	desired := "desired"

	result, err := ApplyUpdateCAS(store, bead.ID, bead.Revision, UpdateOpts{Title: &desired})
	if err != nil {
		t.Fatalf("ApplyUpdateCAS: %v", err)
	}
	if result.Outcome != UpdateCASConflict {
		t.Fatalf("outcome = %q, want %q", result.Outcome, UpdateCASConflict)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != concurrent {
		t.Fatalf("conflict wrote title %q, want concurrent value %q", got.Title, concurrent)
	}
}

func TestApplyUpdateCASRequiresConditionalWriter(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := "desired"
	store := storeWithoutUpdateCAS{Store: backing}

	_, err = ApplyUpdateCAS(store, bead.ID, bead.Revision, UpdateOpts{Title: &desired})
	if !errors.Is(err, ErrConditionalWriteUnsupported) {
		t.Fatalf("ApplyUpdateCAS error = %v, want ErrConditionalWriteUnsupported", err)
	}
	got, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "base" {
		t.Fatalf("unsupported store changed title to %q", got.Title)
	}
}

func TestApplyUpdateCASRejectsUnusableRevision(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	bead, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	desired := "desired"
	_, err = ApplyUpdateCAS(store, bead.ID, 0, UpdateOpts{Title: &desired})
	if err == nil || !strings.Contains(err.Error(), "nonzero opaque token") {
		t.Fatalf("ApplyUpdateCAS revision 0 error = %v, want nonzero-token error", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "base" {
		t.Fatalf("invalid revision changed title to %q", got.Title)
	}
}

func TestApplyUpdateCASFollowsDeclaredResolutionTarget(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := updateCASTargetWrapper{Store: backing}
	desired := "desired"

	result, err := ApplyUpdateCAS(store, bead.ID, bead.Revision, UpdateOpts{Title: &desired})
	if err != nil {
		t.Fatalf("ApplyUpdateCAS: %v", err)
	}
	if result.Outcome != UpdateCASUpdated {
		t.Fatalf("outcome = %q, want %q", result.Outcome, UpdateCASUpdated)
	}
}

func TestApplyUpdateCASTransportErrorIsNotReclassified(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sentinel := errors.New("connection lost after commit")
	store := &updateCASTransportErrorStore{MemStore: backing, err: sentinel}
	desired := "desired"

	_, err = ApplyUpdateCAS(store, bead.ID, bead.Revision, UpdateOpts{Title: &desired})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ApplyUpdateCAS error = %v, want %v", err, sentinel)
	}
	got, err := backing.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != desired {
		t.Fatalf("ambiguous write did not commit test fixture: title = %q", got.Title)
	}

	retry, err := ApplyUpdateCAS(store, bead.ID, bead.Revision, UpdateOpts{Title: &desired})
	if err != nil {
		t.Fatalf("retry ApplyUpdateCAS: %v", err)
	}
	if retry.Outcome != UpdateCASAlreadyApplied {
		t.Fatalf("retry outcome = %q, want %q", retry.Outcome, UpdateCASAlreadyApplied)
	}
}

func TestApplyUpdateCASRequiresAuthoritativeReadback(t *testing.T) {
	t.Parallel()

	backing := NewMemStore()
	bead, err := backing.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &updateCASNoopWriterStore{MemStore: backing}
	desired := "desired"

	_, err = ApplyUpdateCAS(store, bead.ID, bead.Revision, UpdateOpts{Title: &desired})
	if err == nil {
		t.Fatal("ApplyUpdateCAS error = nil, want readback mismatch")
	}
	if got, getErr := backing.Get(bead.ID); getErr != nil || got.Title != "base" {
		t.Fatalf("backing state after false success = (%+v, %v), want unchanged", got, getErr)
	}
}

func TestApplyUpdateCASBdStoreReplayPreservesFullStatusVocabulary(t *testing.T) {
	w := &scriptedBd{id: "ga-1", revision: 2, status: "blocked"}
	store := NewBdStore("/city", w.runner)
	desired := "blocked"

	result, err := ApplyUpdateCAS(store, "ga-1", 1, UpdateOpts{Status: &desired})
	if err != nil {
		t.Fatalf("ApplyUpdateCAS: %v", err)
	}
	if result.Outcome != UpdateCASAlreadyApplied {
		t.Fatalf("outcome = %q, want %q", result.Outcome, UpdateCASAlreadyApplied)
	}
	if w.writeCalls != 0 {
		t.Fatalf("replay ran %d writes, want 0", w.writeCalls)
	}
}

type storeWithoutUpdateCAS struct {
	Store
}

type updateCASTargetWrapper struct {
	Store
}

func (s updateCASTargetWrapper) ConditionalWritesResolveTarget() Store { return s.Store }

type updateCASTransportErrorStore struct {
	*MemStore
	err  error
	done bool
}

func (s *updateCASTransportErrorStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if !s.done {
		s.done = true
		if err := s.MemStore.UpdateIfMatch(id, expectedRevision, opts); err != nil {
			return err
		}
		return s.err
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

type updateCASNoopWriterStore struct {
	*MemStore
}

func (*updateCASNoopWriterStore) UpdateIfMatch(string, int64, UpdateOpts) error {
	return nil
}
