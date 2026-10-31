package molecule

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

const testRootMetaKey = "gc.test_root_meta"

func rootMetadataRecipe(rootOnly bool) *formula.Recipe {
	return &formula.Recipe{
		Name:     "wf",
		RootOnly: rootOnly,
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}
}

func withGraphApply(t *testing.T, enabled bool) {
	t.Helper()
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(enabled)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })
}

// RootMetadata lands on the root only, through the per-bead create path.
func TestInstantiateRootMetadataStampsRootOnly(t *testing.T) {
	withGraphApply(t, false)
	store := beads.NewMemStore()
	rootMeta := map[string]string{testRootMetaKey: "alice"}

	result, err := Instantiate(context.Background(), store, rootMetadataRecipe(false), Options{RootMetadata: rootMeta})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got := root.Metadata[testRootMetaKey]; got != "alice" {
		t.Fatalf("root %s = %q, want alice", testRootMetaKey, got)
	}
	step, err := store.Get(result.IDMapping["wf.step"])
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if got, ok := step.Metadata[testRootMetaKey]; ok {
		t.Fatalf("step carries root metadata %q; RootMetadata must stay on the root", got)
	}
	if len(rootMeta) != 1 {
		t.Fatalf("Instantiate mutated the caller's RootMetadata: %v", rootMeta)
	}
}

// The graph-apply batch path builds the root node on its own; RootMetadata
// must reach it there too.
func TestInstantiateRootMetadataStampsRootOnlyViaGraphApply(t *testing.T) {
	withGraphApply(t, true)
	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}

	if _, err := Instantiate(context.Background(), store, rootMetadataRecipe(false), Options{
		RootMetadata: map[string]string{testRootMetaKey: "alice"},
	}); err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if store.plan == nil {
		t.Fatal("ApplyGraphPlan was not called")
	}
	for _, node := range store.plan.Nodes {
		got, ok := node.Metadata[testRootMetaKey]
		switch node.Key {
		case "wf":
			if got != "alice" {
				t.Fatalf("root node %s = %q, want alice", testRootMetaKey, got)
			}
		default:
			if ok {
				t.Fatalf("node %s carries root metadata %q; RootMetadata must stay on the root", node.Key, got)
			}
		}
	}
}

// An attached root-only wisp (sling --on) is created under its source bead;
// its root is the one RootMetadata stamps, in either create path.
func TestInstantiateRootMetadataStampsAttachedRootOnlyWisp(t *testing.T) {
	for _, graphApply := range []bool{false, true} {
		withGraphApply(t, graphApply)
		store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
		source, err := store.Create(beads.Bead{Title: "Source", Type: "task"})
		if err != nil {
			t.Fatalf("create source: %v", err)
		}
		result, err := Instantiate(context.Background(), store, rootMetadataRecipe(true), Options{
			ParentID:     source.ID,
			RootMetadata: map[string]string{testRootMetaKey: "alice"},
		})
		if err != nil {
			t.Fatalf("graphApply=%v Instantiate: %v", graphApply, err)
		}
		if graphApply {
			if store.plan == nil || len(store.plan.Nodes) != 1 {
				t.Fatalf("graphApply plan = %+v, want the single root node", store.plan)
			}
			if got := store.plan.Nodes[0].Metadata[testRootMetaKey]; got != "alice" {
				t.Fatalf("graph-apply root %s = %q, want alice", testRootMetaKey, got)
			}
			continue
		}
		root, err := store.Get(result.RootID)
		if err != nil {
			t.Fatalf("get root: %v", err)
		}
		if got := root.Metadata[testRootMetaKey]; got != "alice" {
			t.Fatalf("root %s = %q, want alice", testRootMetaKey, got)
		}
	}
}

// No RootMetadata leaves the root exactly as before.
func TestInstantiateWithoutRootMetadataStampsNothing(t *testing.T) {
	withGraphApply(t, false)
	store := beads.NewMemStore()
	result, err := Instantiate(context.Background(), store, rootMetadataRecipe(false), Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if got, ok := root.Metadata[testRootMetaKey]; ok {
		t.Fatalf("root carries %s=%q without RootMetadata", testRootMetaKey, got)
	}
}
