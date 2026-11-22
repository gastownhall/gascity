package molecule

import (
	"cmp"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// Blocker is an existing bead a molecule waits for (Options.BlockedBy), and
// the ready-blocking type of the edge each of its beads waits through. The
// edge is written with no payload: bd, which reads a waits-for edge's gate
// from its payload, reads the copy as a plain all-children gate.
type Blocker struct {
	// ID is the bead waited for.
	ID string
	// Type is the dependency type of the edge: "blocks", "conditional-blocks"
	// or "waits-for". Empty means "blocks".
	Type string
}

// defaultBlockerType is the edge type of a Blocker that names none.
const defaultBlockerType = "blocks"

// BlockerIDs returns the ids of blockers, in order.
func BlockerIDs(blockers []Blocker) []string {
	if len(blockers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		ids = append(ids, blocker.ID)
	}
	return ids
}

// normalizeBlockedBy trims ids, drops empty ones, gives an untyped blocker the
// blocks type, keeps one blocker per id (the first), and sorts by id. A store
// holds at most one ready-blocking edge between two beads, so one per id loses
// no edge a source bead could hold.
func normalizeBlockedBy(blockers []Blocker) []Blocker {
	out := make([]Blocker, 0, len(blockers))
	for _, blocker := range blockers {
		blocker.ID = strings.TrimSpace(blocker.ID)
		if blocker.ID == "" || slices.ContainsFunc(out, func(b Blocker) bool { return b.ID == blocker.ID }) {
			continue
		}
		if blocker.Type == "" {
			blocker.Type = defaultBlockerType
		}
		out = append(out, blocker)
	}
	slices.SortFunc(out, func(a, b Blocker) int { return cmp.Compare(a.ID, b.ID) })
	if len(out) == 0 {
		return nil
	}
	return out
}

// withBlockedByExternalDeps returns external deps that add, from every recipe
// step, root included, one edge of each blocker's own type to that blocker,
// merged after existing. An existing dep on the same (step, target) pair wins,
// so a caller-supplied edge is never duplicated or retyped. A RootOnly recipe
// creates only its root step, so no edge is minted for the steps it leaves
// uncreated.
//
// The teardown tail gets no edge (TeardownTailExclusionFromMembers, the rule a
// run cancel spares it by): teardown runs after the workflow settles, so its
// in-recipe predecessors already hold it while the workflow waits, and a run
// canceled while it waits must leave the tail free to run.
func withBlockedByExternalDeps(recipe *formula.Recipe, existing []ExternalDep, blockedBy []Blocker) []ExternalDep {
	if recipe == nil || len(blockedBy) == 0 {
		return existing
	}
	type pair struct{ step, target string }
	seen := make(map[pair]bool, len(existing))
	for _, dep := range existing {
		seen[pair{dep.StepID, dep.DependsOnID}] = true
	}
	inTeardownTail := recipeTeardownTail(recipe)
	merged := slices.Clone(existing)
	for i, step := range recipe.Steps {
		if recipe.RootOnly && i > 0 {
			break
		}
		if step.ID == "" || inTeardownTail(step) {
			continue
		}
		for _, blocker := range blockedBy {
			key := pair{step.ID, blocker.ID}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, ExternalDep{StepID: step.ID, DependsOnID: blocker.ID, Type: blocker.Type})
		}
	}
	return merged
}

// recipeTeardownTail reports whether a recipe step is in its workflow's
// teardown tail, by the rule TeardownTailExclusionFromMembers applies to the
// beads the steps become: that rule reads only metadata, which a step carries
// onto its bead.
func recipeTeardownTail(recipe *formula.Recipe) func(formula.RecipeStep) bool {
	members := make([]beads.Bead, 0, len(recipe.Steps))
	for _, step := range recipe.Steps {
		members = append(members, beads.Bead{Metadata: step.Metadata})
	}
	inTail := TeardownTailExclusionFromMembers(members)
	return func(step formula.RecipeStep) bool {
		return inTail(beads.Bead{Metadata: step.Metadata})
	}
}
