package molecule

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/formula"
)

// blockedByDepType is the dependency type BlockedBy embeds: the plain
// ready-blocking edge, so a blocker releases the workflow by the store's own
// readiness rule.
const blockedByDepType = "blocks"

// normalizeBlockedBy trims, drops empty entries, dedupes, and sorts ids.
func normalizeBlockedBy(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	slices.Sort(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// withBlockedByExternalDeps returns external deps that add one blocks edge
// from every recipe step, root included, to every id in blockedBy, merged
// after existing. An existing dep on the same (step, target) pair wins, so a
// caller-supplied edge is never duplicated or retyped. A RootOnly recipe
// creates only its root step, so no edge is minted for the steps it leaves
// uncreated.
func withBlockedByExternalDeps(recipe *formula.Recipe, existing []ExternalDep, blockedBy []string) []ExternalDep {
	if recipe == nil || len(blockedBy) == 0 {
		return existing
	}
	type pair struct{ step, target string }
	seen := make(map[pair]bool, len(existing))
	for _, dep := range existing {
		seen[pair{dep.StepID, dep.DependsOnID}] = true
	}
	merged := slices.Clone(existing)
	for i, step := range recipe.Steps {
		if recipe.RootOnly && i > 0 {
			break
		}
		if step.ID == "" {
			continue
		}
		for _, target := range blockedBy {
			key := pair{step.ID, target}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, ExternalDep{StepID: step.ID, DependsOnID: target, Type: blockedByDepType})
		}
	}
	return merged
}
