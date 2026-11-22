package molecule

import (
	"context"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/formula"
)

func blockedByTestRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.a", Title: "A", Type: "task"},
			{ID: "wf.b", Title: "B", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.a", DependsOnID: "wf", Type: "parent-child"},
			{StepID: "wf.b", DependsOnID: "wf", Type: "parent-child"},
			{StepID: "wf.b", DependsOnID: "wf.a", Type: "blocks"},
		},
	}
}

// TestInstantiateBlockedByWiresEveryStep pins Options.BlockedBy on the
// sequential path: every created bead, root included, gets one blocks edge
// per blocker, an ExternalDep on the same pair is not duplicated, and Result
// reports the normalized set.
func TestInstantiateBlockedByWiresEveryStep(t *testing.T) {
	store := beads.NewMemStore()
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(false)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })
	first, err := store.Create(beads.Bead{Title: "first blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(beads.Bead{Title: "second blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), blockedByTestRecipe(), Options{
		ExternalDeps: []ExternalDep{{StepID: "wf.a", DependsOnID: first.ID, Type: "blocks"}},
		BlockedBy:    []string{" " + second.ID, first.ID, second.ID, ""},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	want := []string{first.ID, second.ID}
	slices.Sort(want)
	if !slices.Equal(result.BlockedBy, want) {
		t.Errorf("Result.BlockedBy = %v, want %v", result.BlockedBy, want)
	}
	for _, stepID := range []string{"wf", "wf.a", "wf.b"} {
		beadID := result.RootID
		if stepID != "wf" {
			beadID = result.IDMapping[stepID]
		}
		deps, err := store.DepList(beadID, "down")
		if err != nil {
			t.Fatalf("DepList(%s): %v", beadID, err)
		}
		for _, blocker := range want {
			n := 0
			for _, d := range deps {
				if d.DependsOnID == blocker && d.Type == "blocks" {
					n++
				}
			}
			if n != 1 {
				t.Errorf("step %s (%s) has %d blocks edges on %s, want 1", stepID, beadID, n, blocker)
			}
		}
	}
}

// TestInstantiateGraphApplyIncludesBlockedBy pins Options.BlockedBy on the
// graph-apply path: the plan carries one blocks edge per step and blocker.
func TestInstantiateGraphApplyIncludesBlockedBy(t *testing.T) {
	store := &graphApplySpyStore{MemStore: beads.NewMemStore()}
	blocker, err := store.Create(beads.Bead{Title: "Blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(true)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), blockedByTestRecipe(), Options{BlockedBy: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if store.plan == nil {
		t.Fatal("ApplyGraphPlan was not called")
	}
	for _, stepID := range []string{"wf", "wf.a", "wf.b"} {
		assertGraphPlanEdgeCount(t, store.plan, stepID, "", blocker.ID, "blocks", 1)
	}
	if !slices.Equal(result.BlockedBy, []string{blocker.ID}) {
		t.Errorf("Result.BlockedBy = %v, want [%s]", result.BlockedBy, blocker.ID)
	}
}

// TestInstantiateWithoutBlockedByAddsNoEdges pins that an empty BlockedBy
// leaves the molecule's dependencies to the recipe alone.
func TestInstantiateWithoutBlockedByAddsNoEdges(t *testing.T) {
	store := beads.NewMemStore()
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(false)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), blockedByTestRecipe(), Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if result.BlockedBy != nil {
		t.Errorf("Result.BlockedBy = %v, want nil", result.BlockedBy)
	}
	deps, err := store.DepList(result.IDMapping["wf.a"], "down")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deps {
		if d.Type == "blocks" {
			t.Errorf("entry step has a blocks edge on %s without BlockedBy", d.DependsOnID)
		}
	}
}

func assertBlockedByBlocksEdge(t *testing.T, store *beads.MemStore, beadID, targetID string) {
	t.Helper()
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", beadID, err)
	}
	for _, dep := range deps {
		if dep.DependsOnID == targetID && dep.Type == "blocks" {
			return
		}
	}
	t.Errorf("bead %s has no blocks edge on %s; deps=%v", beadID, targetID, deps)
}

// TestInstantiateBlockedByMergesAndDedupesWithExternalDeps pins that a
// caller-supplied ExternalDep on the same (step, target) pair as a BlockedBy
// expansion wins: no duplicate edge is minted and its type is kept.
func TestInstantiateBlockedByMergesAndDedupesWithExternalDeps(t *testing.T) {
	store := beads.NewMemStore()
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(false)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(beads.Bead{Title: "other", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), blockedByTestRecipe(), Options{
		BlockedBy:    []string{blocker.ID, other.ID},
		ExternalDeps: []ExternalDep{{StepID: "wf.a", DependsOnID: blocker.ID, Type: "waits-for"}},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	first := result.IDMapping["wf.a"]
	deps, err := store.DepList(first, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", first, err)
	}
	blockerEdges := 0
	for _, dep := range deps {
		if dep.DependsOnID != blocker.ID {
			continue
		}
		blockerEdges++
		if dep.Type != "waits-for" {
			t.Errorf("entry step's edge on %s is type %q; the caller-supplied waits-for edge must win over the BlockedBy blocks expansion", blocker.ID, dep.Type)
		}
	}
	if blockerEdges != 1 {
		t.Fatalf("entry step has %d edges on %s, want exactly the one deduped edge", blockerEdges, blocker.ID)
	}
	assertBlockedByBlocksEdge(t, store, first, other.ID)
	assertBlockedByBlocksEdge(t, store, result.IDMapping["wf.b"], blocker.ID)
	assertBlockedByBlocksEdge(t, store, result.RootID, blocker.ID)
}

// TestInstantiateBlockedByRootOnlyCoversCreatedRootOnly pins that a RootOnly
// recipe wires the blocker onto the one bead it creates.
func TestInstantiateBlockedByRootOnlyCoversCreatedRootOnly(t *testing.T) {
	store := beads.NewMemStore()
	prev := IsGraphApplyEnabled()
	SetGraphApplyEnabled(false)
	t.Cleanup(func() { SetGraphApplyEnabled(prev) })
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	recipe := blockedByTestRecipe()
	recipe.RootOnly = true

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), recipe, Options{BlockedBy: []string{blocker.ID}})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if result.Created != 1 {
		t.Fatalf("Created = %d, want 1 for a root-only recipe", result.Created)
	}
	assertBlockedByBlocksEdge(t, store, result.RootID, blocker.ID)
}

// TestInstantiateFragmentRejectsBlockedBy pins that a fragment, which
// attaches into an existing workflow, refuses a launch-and-wait hold.
func TestInstantiateFragmentRejectsBlockedBy(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "workflow", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = InstantiateFragment(context.Background(), beadstest.GraphMolecule(store), &formula.FragmentRecipe{
		Name:  "fragment",
		Steps: []formula.RecipeStep{{ID: "fragment.item", Title: "Item", Type: "task"}},
	}, FragmentOptions{RootID: root.ID, BlockedBy: []string{"gc-1"}})
	if err == nil {
		t.Fatal("InstantiateFragment accepted BlockedBy; a fragment attaches into an existing workflow and must reject a launch-and-wait hold")
	}
}
