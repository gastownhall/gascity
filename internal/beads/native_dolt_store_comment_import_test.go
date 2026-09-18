package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltStoreAtomicCommentImportCommitsRowsReceiptAndFieldsTogether(t *testing.T) {
	t.Parallel()

	storage := newNativeDoltMemStorage()
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	title := "human title"
	comments := []ImportedComment{
		{ExternalID: "IC_2", Author: "github-human", Text: "second", CreatedAt: time.Date(2026, 9, 2, 12, 35, 0, 0, time.UTC)},
		{ExternalID: "IC_1", Author: "github-human", Text: "first", CreatedAt: time.Date(2026, 9, 2, 12, 34, 0, 0, time.UTC)},
	}

	result, err := ApplyUpdateCASWithComments(
		store, created.ID, created.Revision, UpdateOpts{Title: &title}, comments,
	)
	if err != nil {
		t.Fatalf("ApplyUpdateCASWithComments: %v", err)
	}
	if result.Outcome != UpdateCASUpdated || result.Revision == created.Revision {
		t.Fatalf("result = %+v, want updated with fresh revision", result)
	}
	if len(storage.comments[created.ID]) != 2 {
		t.Fatalf("comments = %#v, want two rows", storage.comments[created.ID])
	}
	bound, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get bound: %v", err)
	}
	if bound.Title != title || bound.Metadata[ImportedCommentIDsMetadataKey] != `["IC_1","IC_2"]` {
		t.Fatalf("bound = %+v", bound)
	}
	replay, err := ApplyUpdateCASWithComments(
		store, created.ID, created.Revision, UpdateOpts{Title: &title}, comments,
	)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Outcome != UpdateCASAlreadyApplied || len(storage.comments[created.ID]) != 2 {
		t.Fatalf("replay=%+v comments=%#v", replay, storage.comments[created.ID])
	}
}

func TestNativeUpdatesForCommentImportPreserveMixedJSONMetadataSiblings(t *testing.T) {
	t.Parallel()

	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	rawMetadata := json.RawMessage(`{"bool_sibling":true,"object_sibling":{"nested":1}}`)
	updates, err := store.nativeUpdatesPreservingRawMetadata(
		context.Background(),
		nativeIssueGetterFunc(func(context.Context, string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: "gc-1", Metadata: rawMetadata}, nil
		}),
		"gc-1",
		rawMetadata,
		UpdateOpts{Metadata: map[string]string{ImportedCommentIDsMetadataKey: `["IC_1"]`}},
	)
	if err != nil {
		t.Fatalf("nativeUpdatesPreservingRawMetadata: %v", err)
	}
	raw, ok := updates["metadata"].(json.RawMessage)
	if !ok {
		t.Fatalf("metadata update type = %T, want json.RawMessage", updates["metadata"])
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode metadata update: %v", err)
	}
	if string(values["bool_sibling"]) != "true" || string(values["object_sibling"]) != `{"nested":1}` {
		t.Fatalf("mixed metadata siblings changed type: %s", raw)
	}
	if string(values[ImportedCommentIDsMetadataKey]) != `"[\"IC_1\"]"` {
		t.Fatalf("comment receipt = %s", values[ImportedCommentIDsMetadataKey])
	}
}

func TestNativeDoltStoreAtomicCommentImportRollsBackOnCommentReadbackFailure(t *testing.T) {
	t.Parallel()

	storage := newNativeDoltMemStorage()
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "base"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	storage.commentReadErr = errors.New("comment readback unavailable")
	comments := []ImportedComment{{
		ExternalID: "IC_1",
		Author:     "github-human",
		Text:       "must roll back",
		CreatedAt:  time.Date(2026, 9, 2, 12, 34, 0, 0, time.UTC),
	}}

	_, err = ApplyUpdateCASWithComments(store, created.ID, created.Revision, UpdateOpts{}, comments)
	if err == nil || !errors.Is(err, storage.commentReadErr) {
		t.Fatalf("error = %v, want readback failure", err)
	}
	if len(storage.comments[created.ID]) != 0 {
		t.Fatalf("comments committed after failure: %#v", storage.comments[created.ID])
	}
	unchanged, getErr := store.Get(created.ID)
	if getErr != nil {
		t.Fatalf("Get unchanged: %v", getErr)
	}
	if unchanged.Revision != created.Revision || unchanged.Metadata[ImportedCommentIDsMetadataKey] != "" {
		t.Fatalf("receipt/revision committed after failure: %+v", unchanged)
	}
}

type nativeIssueGetterFunc func(context.Context, string) (*beadslib.Issue, error)

func (fn nativeIssueGetterFunc) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	return fn(ctx, id)
}
