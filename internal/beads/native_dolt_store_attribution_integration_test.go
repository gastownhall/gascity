//go:build integration

package beads

import (
	"context"
	"encoding/json"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

const attributionTestActor = "order:attribution-test"

func TestNativeDoltStoreCreateStampsCreatedByFromBeadsActor(t *testing.T) {
	t.Setenv("BEADS_ACTOR", attributionTestActor)
	store, storage := openRealNativeDoltStoreForMetadata(t, nativeDoltStoreActor)
	created, err := store.Create(Bead{Title: "mail", Type: "task", From: "human", Assignee: "mayor"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertCreatedBy(t, storage, created.ID, attributionTestActor)
}

func TestNativeDoltStoreTxCreateStampsCreatedByFromBeadsActor(t *testing.T) {
	t.Setenv("BEADS_ACTOR", attributionTestActor)
	store, storage := openRealNativeDoltStoreForMetadata(t, nativeDoltStoreActor)
	var created Bead
	err := store.Tx("attribution test", func(tx Tx) error {
		var err error
		created, err = tx.Create(Bead{Title: "tracking", Type: "task"})
		return err
	})
	if err != nil {
		t.Fatalf("Tx create: %v", err)
	}
	assertCreatedBy(t, storage, created.ID, attributionTestActor)
}

func TestNativeDoltStoreGraphApplyStampsCreatedByFromBeadsActor(t *testing.T) {
	t.Setenv("BEADS_ACTOR", attributionTestActor)
	store, storage := openRealNativeDoltStoreForMetadata(t, nativeDoltStoreActor)
	result, err := store.ApplyGraphPlan(context.Background(), &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "molecule root", Type: "task"},
			{Key: "step", Title: "step", Type: "task", ParentKey: "root"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	for key, id := range result.IDs {
		t.Run(key, func(t *testing.T) { assertCreatedBy(t, storage, id, attributionTestActor) })
	}
}

func TestNativeDoltStoreCreateMirrorsFromIntoMetadata(t *testing.T) {
	store, storage := openRealNativeDoltStoreForMetadata(t, nativeDoltStoreActor)
	created, err := store.Create(Bead{Title: "mail", Type: "task", From: "human", Assignee: "mayor"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	issue, err := storage.GetIssue(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	var metadata map[string]any
	if len(issue.Metadata) > 0 && string(issue.Metadata) != "null" {
		if err := json.Unmarshal(issue.Metadata, &metadata); err != nil {
			t.Fatalf("unmarshaling persisted metadata %s: %v", issue.Metadata, err)
		}
	}
	if got := metadata["from"]; got != "human" {
		t.Errorf(`metadata["from"] = %#v (metadata %s), want "human" (the shape BdStore writes)`, got, issue.Metadata)
	}
}

func TestNativeDoltStoreReadsSenderFromBdStoreWrittenMetadata(t *testing.T) {
	store, storage := openRealNativeDoltStoreForMetadata(t, nativeDoltStoreActor)
	seeded := &beadslib.Issue{
		ID: "gc-bdstoremail", Title: "written by BdStore", Status: beadslib.StatusOpen,
		Priority: 2, IssueType: beadslib.TypeTask, Assignee: "mayor",
		Metadata: json.RawMessage(`{"from":"human"}`),
	}
	if err := storage.CreateIssue(context.Background(), seeded, "controller"); err != nil {
		t.Fatalf("seed BdStore-shaped row: %v", err)
	}
	got, err := store.Get(seeded.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.From != "human" {
		t.Errorf("From = %q, want %q read back from metadata[\"from\"]", got.From, "human")
	}
}

func assertCreatedBy(t *testing.T, storage beadslib.Storage, id, want string) {
	t.Helper()
	issue, err := storage.GetIssue(context.Background(), id)
	if err != nil {
		t.Fatalf("GetIssue(%s): %v", id, err)
	}
	if issue == nil {
		t.Fatalf("bead %q not found", id)
	}
	if issue.CreatedBy != want {
		t.Errorf("%s created_by = %q, want %q (the actor bd would have stamped)", id, issue.CreatedBy, want)
	}
}
