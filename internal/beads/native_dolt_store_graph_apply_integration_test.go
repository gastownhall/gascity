//go:build integration

package beads

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/beads/issueops"
)

// TestNativeDoltStoreApplyGraphPlanRealBackendAtomic proves the
// issueops.BatchApplier-based ApplyGraphPlanWithStorage against the REAL
// upstream native beads storage: creates, the explicit edge, the parent link
// (PARENT-ON-CREATE's one spelling), the metadata-ref splice, and the deferred
// post-create assignment all land from ONE request. The unit-level
// TestNativeApplyGraphPlanComposesOneBatchRequest pins the REQUEST this route
// composes; this test pins what the real role does with it.
func TestNativeDoltStoreApplyGraphPlanRealBackendAtomic(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "graph-apply-real")

	result, err := store.ApplyGraphPlanWithStorage(context.Background(), &GraphApplyPlan{
		CommitMessage: "gc: real graph apply",
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "Root"},
			{Key: "blocker", Title: "Blocker"},
			{
				Key:               "child",
				Title:             "Child",
				ParentKey:         "root",
				Assignee:          "gascity/worker",
				AssignAfterCreate: true,
				MetadataRefs:      map[string]string{"gc.root_bead_id": "root", "gc.blocker_id": "blocker"},
			},
		},
		Edges: []GraphApplyEdge{{FromKey: "child", ToKey: "blocker"}},
	}, StorageDefault)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}

	root, err := store.Get(result.IDs["root"])
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	blocker, err := store.Get(result.IDs["blocker"])
	if err != nil {
		t.Fatalf("Get blocker: %v", err)
	}
	child, err := store.Get(result.IDs["child"])
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}

	if child.Assignee != "gascity/worker" {
		t.Errorf("child.Assignee = %q, want gascity/worker (the deferred update item)", child.Assignee)
	}
	if child.Metadata["gc.root_bead_id"] != root.ID || child.Metadata["gc.blocker_id"] != blocker.ID {
		t.Errorf("child metadata refs = %#v, want root/blocker ids (the role's own CreateItem.MetadataRefs splice)", child.Metadata)
	}
	if child.ParentID != root.ID {
		t.Errorf("child.ParentID = %q, want %q (PARENT-ON-CREATE's dep_add)", child.ParentID, root.ID)
	}
	assertNativeDependency(t, child.Dependencies, child.ID, root.ID, "parent-child")
	assertNativeDependency(t, child.Dependencies, child.ID, blocker.ID, "blocks")
}

// TestNativeDoltStoreApplyGraphPlanMidPlanFailureLeavesZeroRows proves the
// route's ONE-REQUEST atomicity against the REAL backend: a plan that is
// structurally valid (passes validateGraphApplyPlan) but whose edges form a
// hierarchy conflict only the role's END GATE can see is refused with
// *issueops.DependencyHierarchyConflictError, and NOTHING it would have
// created survives — not the edges, and not the issues the same request
// would have minted.
//
// The shape mirrors the beads-upstream conformance fixture
// (RunBatchApplyEndGateRefusesAHierarchyTheRequestBuilt): a blocking edge
// child->grand lands first (legal when written, since no parent-child edge
// exists yet), then the two parent-child edges parent->grand and child->parent
// complete an ancestor chain the blocking edge now contradicts. Only a
// whole-request re-validation after every item lands catches it — the exact
// property the at-or-under-cap BatchApplier route depends on instead of
// re-implementing its own pairwise pre-check. (The pairwise check,
// nativeGraphApplyParentDepPairs and friends, still exists, but only for the
// OVER-CAP local-transaction fallback below, which has no end gate of its
// own to lean on.)
func TestNativeDoltStoreApplyGraphPlanMidPlanFailureLeavesZeroRows(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "graph-apply-midfail")
	label := "graph-apply-midfail-marker"

	plan := &GraphApplyPlan{
		CommitMessage: "gc: graph apply hierarchy conflict",
		Nodes: []GraphApplyNode{
			{Key: "grand", Title: "Grand", Labels: []string{label}},
			{Key: "parent", Title: "Parent", Labels: []string{label}, ParentKey: "grand"},
			{Key: "child", Title: "Child", Labels: []string{label}, ParentKey: "parent"},
		},
		// child depends on (is blocked by) its own grandparent: legal when
		// written, illegal once the parent-child chain below completes it.
		Edges: []GraphApplyEdge{{FromKey: "child", ToKey: "grand"}},
	}

	_, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err == nil {
		t.Fatal("ApplyGraphPlanWithStorage: a hierarchy-conflicting plan was accepted")
	}
	var hierarchy *issueops.DependencyHierarchyConflictError
	if !errors.As(err, &hierarchy) && !errors.Is(err, issueops.ErrDependencyCycle) {
		t.Fatalf("error = %v, want *issueops.DependencyHierarchyConflictError or issueops.ErrDependencyCycle from the end gate", err)
	}

	survivors, err := store.ListByLabel(label, 10, IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(survivors) != 0 {
		ids := make([]string, 0, len(survivors))
		for _, b := range survivors {
			ids = append(ids, b.ID)
		}
		t.Fatalf("%d bead(s) survived a refused graph apply (%v); want 0: the whole request is one transaction and must leave nothing behind", len(survivors), ids)
	}
}

// TestNativeDoltStoreApplyGraphPlanOverCapSucceedsAtomicallyOnEmbeddedDolt is
// the reviewer's G3 HIGH-2 probe: a plan whose batch representation exceeds
// issueops.MaxApplyBatchItems (41 nodes, 40 of them carrying both a
// ParentKey and AssignAfterCreate, expanding to 41 creates + 40 parent-child
// dep_adds + 40 deferred assignee updates = 121 items) used to hard-fail with
// *GraphApplyTooLargeError on EVERY backend, including this real embedded
// one that has a working beadslib.RunInTransaction and therefore never
// needed BatchApplier's cap at all. It must now succeed, atomically, through
// applyGraphPlanOverCapInTransaction.
func TestNativeDoltStoreApplyGraphPlanOverCapSucceedsAtomicallyOnEmbeddedDolt(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "graph-apply-over-cap")

	const childCount = 40
	nodes := make([]GraphApplyNode, 0, childCount+1)
	nodes = append(nodes, GraphApplyNode{Key: "root", Title: "Root"})
	for i := 0; i < childCount; i++ {
		nodes = append(nodes, GraphApplyNode{
			Key:               fmt.Sprintf("n%d", i),
			Title:             fmt.Sprintf("Node %d", i),
			ParentKey:         "root",
			Assignee:          "gascity/worker",
			AssignAfterCreate: true,
		})
	}
	plan := &GraphApplyPlan{CommitMessage: "gc: over-cap graph apply", Nodes: nodes}

	wantItems := (childCount + 1) + childCount + childCount // creates + parent links + assigns
	if wantItems != 121 {
		t.Fatalf("test construction error: wantItems = %d, want 121", wantItems)
	}
	if wantItems <= issueops.MaxApplyBatchItems {
		t.Fatalf("test construction error: %d items does not exceed the %d-item cap", wantItems, issueops.MaxApplyBatchItems)
	}

	result, err := store.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v, want atomic success through the local-transaction fallback", err)
	}
	if len(result.IDs) != len(nodes) {
		t.Fatalf("len(result.IDs) = %d, want %d", len(result.IDs), len(nodes))
	}

	root, err := store.Get(result.IDs["root"])
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	for i := 0; i < childCount; i++ {
		key := fmt.Sprintf("n%d", i)
		child, err := store.Get(result.IDs[key])
		if err != nil {
			t.Fatalf("Get %s: %v", key, err)
		}
		if child.ParentID != root.ID {
			t.Errorf("%s.ParentID = %q, want %q", key, child.ParentID, root.ID)
		}
		if child.Assignee != "gascity/worker" {
			t.Errorf("%s.Assignee = %q, want gascity/worker (the deferred update)", key, child.Assignee)
		}
		assertNativeDependency(t, child.Dependencies, child.ID, root.ID, "parent-child")
	}
}
