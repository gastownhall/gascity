package molecule

import (
	"context"
	"os"
	"path/filepath"
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
		BlockedBy:    []Blocker{{ID: " " + second.ID}, {ID: first.ID}, {ID: second.ID}, {ID: ""}},
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	want := []Blocker{{ID: first.ID, Type: "blocks"}, {ID: second.ID, Type: "blocks"}}
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
				if d.DependsOnID == blocker.ID && d.Type == "blocks" {
					n++
				}
			}
			if n != 1 {
				t.Errorf("step %s (%s) has %d blocks edges on %s, want 1", stepID, beadID, n, blocker.ID)
			}
		}
	}
}

// blockedByTypedBlockers creates one open bead per ready-blocking edge type
// and returns them as blockers of that type.
func blockedByTypedBlockers(t *testing.T, store beads.Store) []Blocker {
	t.Helper()
	var blockers []Blocker
	for _, depType := range []string{"blocks", "conditional-blocks", "waits-for"} {
		b, err := store.Create(beads.Bead{Title: depType + " target", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		blockers = append(blockers, Blocker{ID: b.ID, Type: depType})
	}
	return blockers
}

// TestInstantiateBlockedByKeepsEachBlockersEdgeType pins that each blocker is
// wired with its own edge type, not retyped to blocks: a store judges a
// waits-for edge by its target's children, so only the source's own type holds
// the workflow by the rule that holds the source bead.
func TestInstantiateBlockedByKeepsEachBlockersEdgeType(t *testing.T) {
	store := beads.NewMemStore()
	blockers := blockedByTypedBlockers(t, store)

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), blockedByTestRecipe(), Options{BlockedBy: blockers})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if !slices.Equal(result.BlockedBy, blockers) {
		t.Errorf("Result.BlockedBy = %v, want %v", result.BlockedBy, blockers)
	}
	for _, beadID := range []string{result.RootID, result.IDMapping["wf.a"], result.IDMapping["wf.b"]} {
		deps, err := store.DepList(beadID, "down")
		if err != nil {
			t.Fatalf("DepList(%s): %v", beadID, err)
		}
		for _, blocker := range blockers {
			if !slices.Contains(deps, beads.Dep{IssueID: beadID, DependsOnID: blocker.ID, Type: blocker.Type}) {
				t.Errorf("bead %s has no %s edge on %s; deps=%v", beadID, blocker.Type, blocker.ID, deps)
			}
		}
	}
}

// TestInstantiateWithoutBlockedByAddsNoEdges pins that an empty BlockedBy
// leaves the molecule's dependencies to the recipe alone.
func TestInstantiateWithoutBlockedByAddsNoEdges(t *testing.T) {
	store := beads.NewMemStore()

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
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(beads.Bead{Title: "other", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), blockedByTestRecipe(), Options{
		BlockedBy:    []Blocker{{ID: blocker.ID}, {ID: other.ID}},
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
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	recipe := blockedByTestRecipe()
	recipe.RootOnly = true

	result, err := Instantiate(context.Background(), beadstest.GraphMolecule(store), recipe, Options{BlockedBy: []Blocker{{ID: blocker.ID}}})
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
	}, FragmentOptions{RootID: root.ID, BlockedBy: []Blocker{{ID: "gc-1"}}})
	if err == nil {
		t.Fatal("InstantiateFragment accepted BlockedBy; a fragment attaches into an existing workflow and must reject a launch-and-wait hold")
	}
}

// blockedByTeardownFormula is a scoped graph.v2 workflow whose cleanup is
// teardown work run after the body settles, retry-managed so its first
// attempt carries no gc.scope_role, only the step's gc.step_id.
const blockedByTeardownFormula = `
formula = "held-teardown"
version = 2
contract = "graph.v2"

[[steps]]
id = "body"
title = "Body"
needs = ["work"]
metadata = { "gc.kind" = "scope", "gc.scope_role" = "body", "gc.scope_name" = "work" }

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.scope_ref" = "body", "gc.scope_role" = "member", "gc.on_fail" = "abort_scope" }

[[steps]]
id = "cleanup"
title = "Cleanup"
needs = ["body"]
metadata = { "gc.kind" = "cleanup", "gc.scope_ref" = "body", "gc.scope_role" = "teardown" }

[steps.retry]
max_attempts = 3
`

// TestInstantiateBlockedBySparesTheTeardownTail pins that a held workflow's
// teardown tail carries no blocker edge, so a run canceled while it waits
// (which closes everything but the tail, as the run cancel does) leaves its
// teardown free to run although the blocker is still open. The tail is the
// rule the cancel spares it by: the teardown step and every attempt of it.
func TestInstantiateBlockedBySparesTheTeardownTail(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "held-teardown.toml"), []byte(blockedByTeardownFormula), 0o644); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	target := beadstest.GraphMolecule(store)

	result, err := Cook(context.Background(), target, "held-teardown", []string{dir}, Options{BlockedBy: []Blocker{{ID: blocker.ID}}})
	if err != nil {
		t.Fatalf("Cook: %v", err)
	}
	tail := map[string]bool{
		result.IDMapping["held-teardown.cleanup"]:           true,
		result.IDMapping["held-teardown.cleanup.attempt.1"]: true,
	}
	for stepID, beadID := range result.IDMapping {
		bead, err := store.Get(beadID)
		if err != nil {
			t.Fatalf("Get(%s): %v", beadID, err)
		}
		if bead.Metadata["gc.kind"] == "spec" {
			continue
		}
		deps, err := store.DepList(beadID, "down")
		if err != nil {
			t.Fatalf("DepList(%s): %v", beadID, err)
		}
		held := slices.ContainsFunc(deps, func(d beads.Dep) bool { return d.DependsOnID == blocker.ID })
		if tail[beadID] && held {
			t.Errorf("teardown tail step %s (%s) waits on blocker %s", stepID, beadID, blocker.ID)
		}
		if !tail[beadID] && !held {
			t.Errorf("step %s (%s) does not wait on blocker %s", stepID, beadID, blocker.ID)
		}
	}

	exclude, err := TeardownTailExclusion(target, result.RootID)
	if err != nil {
		t.Fatalf("TeardownTailExclusion: %v", err)
	}
	if _, err := CloseSubtreeWithMetadataExcept(target, result.RootID, map[string]string{"gc.outcome": "canceled"}, exclude); err != nil {
		t.Fatalf("canceling the held workflow: %v", err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	attempt := result.IDMapping["held-teardown.cleanup.attempt.1"]
	if !slices.ContainsFunc(ready, func(b beads.Bead) bool { return b.ID == attempt }) {
		t.Errorf("teardown attempt %s is not Ready after the held workflow was canceled while %s is open", attempt, blocker.ID)
	}
}
