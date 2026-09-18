package beads

import (
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"
)

func TestApplyUpdateCASWithCommentsUpdatesAndReplaysWithoutDuplicateImport(t *testing.T) {
	t.Parallel()

	store := &atomicCommentCASTestStore{MemStore: NewMemStore()}
	bead, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "human title"
	comments := []ImportedComment{{
		ExternalID: "IC_1",
		Author:     "github-human",
		Text:       "human comment",
		CreatedAt:  time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC),
	}}

	result, err := ApplyUpdateCASWithComments(
		store, bead.ID, bead.Revision, UpdateOpts{Title: &title}, comments,
	)
	if err != nil {
		t.Fatalf("ApplyUpdateCASWithComments: %v", err)
	}
	if result.Outcome != UpdateCASUpdated || result.Revision == bead.Revision {
		t.Fatalf("result = %+v, want updated with fresh revision", result)
	}
	if store.writeCalls != 1 || len(store.imported) != 1 {
		t.Fatalf("writes=%d imported=%#v, want one atomic import", store.writeCalls, store.imported)
	}

	replay, err := ApplyUpdateCASWithComments(
		store, bead.ID, bead.Revision, UpdateOpts{Title: &title}, comments,
	)
	if err != nil {
		t.Fatalf("replay ApplyUpdateCASWithComments: %v", err)
	}
	if replay.Outcome != UpdateCASAlreadyApplied || replay.Revision != result.Revision {
		t.Fatalf("replay = %+v, want already_applied at revision %d", replay, result.Revision)
	}
	if store.writeCalls != 1 || len(store.imported) != 1 {
		t.Fatalf("replay duplicated write/import: writes=%d imported=%#v", store.writeCalls, store.imported)
	}
}

func TestApplyUpdateCASWithCommentsStaleMissingCommentConflictsWithoutWrite(t *testing.T) {
	t.Parallel()

	store := &atomicCommentCASTestStore{MemStore: NewMemStore()}
	bead, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	concurrent := "concurrent"
	if err := store.Update(bead.ID, UpdateOpts{Title: &concurrent}); err != nil {
		t.Fatalf("concurrent Update: %v", err)
	}
	comments := []ImportedComment{{
		ExternalID: "IC_missing",
		Author:     "github-human",
		Text:       "must not import",
		CreatedAt:  time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC),
	}}

	result, err := ApplyUpdateCASWithComments(store, bead.ID, bead.Revision, UpdateOpts{}, comments)
	if err != nil {
		t.Fatalf("ApplyUpdateCASWithComments: %v", err)
	}
	if result.Outcome != UpdateCASConflict || store.writeCalls != 0 || len(store.imported) != 0 {
		t.Fatalf("result=%+v writes=%d imported=%#v, want clean conflict", result, store.writeCalls, store.imported)
	}
}

func TestApplyUpdateCASWithCommentsAmbiguousCommitIsReplaySafe(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("ambiguous transport")
	store := &atomicCommentCASTestStore{MemStore: NewMemStore(), returnAfterApply: sentinel}
	bead, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	comments := []ImportedComment{{
		ExternalID: "IC_1",
		Author:     "github-human",
		Text:       "human comment",
		CreatedAt:  time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC),
	}}

	if _, err := ApplyUpdateCASWithComments(store, bead.ID, bead.Revision, UpdateOpts{}, comments); !errors.Is(err, sentinel) {
		t.Fatalf("first error = %v, want %v", err, sentinel)
	}
	result, err := ApplyUpdateCASWithComments(store, bead.ID, bead.Revision, UpdateOpts{}, comments)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if result.Outcome != UpdateCASAlreadyApplied || store.writeCalls != 1 || len(store.imported) != 1 {
		t.Fatalf("retry result=%+v writes=%d imported=%#v", result, store.writeCalls, store.imported)
	}
}

func TestApplyUpdateCASWithCommentsRequiresAtomicCapability(t *testing.T) {
	t.Parallel()

	store := NewMemStore()
	bead, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	comments := []ImportedComment{{
		ExternalID: "IC_1",
		Author:     "github-human",
		Text:       "human comment",
		CreatedAt:  time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC),
	}}

	_, err = ApplyUpdateCASWithComments(store, bead.ID, bead.Revision, UpdateOpts{}, comments)
	if !errors.Is(err, ErrAtomicCommentImportUnsupported) {
		t.Fatalf("error = %v, want ErrAtomicCommentImportUnsupported", err)
	}
}

type atomicCommentCASTestStore struct {
	*MemStore
	writeCalls       int
	imported         []ImportedComment
	returnAfterApply error
}

func (s *atomicCommentCASTestStore) UpdateWithCommentsIfMatch(
	id string,
	expectedRevision int64,
	opts UpdateOpts,
	comments []ImportedComment,
) error {
	s.writeCalls++
	current, err := s.Get(id)
	if err != nil {
		return err
	}
	if current.Revision != expectedRevision {
		return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: current.Revision}
	}
	var ids []string
	if raw := current.Metadata[ImportedCommentIDsMetadataKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &ids); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(ids)+len(comments))
	for _, externalID := range ids {
		seen[externalID] = struct{}{}
	}
	for _, comment := range comments {
		if _, ok := seen[comment.ExternalID]; ok {
			continue
		}
		seen[comment.ExternalID] = struct{}{}
		ids = append(ids, comment.ExternalID)
		s.imported = append(s.imported, comment)
	}
	sort.Strings(ids)
	rawIDs, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if opts.Metadata == nil {
		opts.Metadata = map[string]string{}
	}
	opts.Metadata[ImportedCommentIDsMetadataKey] = string(rawIDs)
	if err := s.UpdateIfMatch(id, expectedRevision, opts); err != nil {
		return err
	}
	err = s.returnAfterApply
	s.returnAfterApply = nil
	return err
}
