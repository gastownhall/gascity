package api

import (
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// runStepDependencyEdges returns each member's step-ordering prerequisite
// edges (prerequisite ID -> dependent ID), drawn from the real bead
// Dependencies and Needs fields the run projection fold preserves. Edges to
// a bead outside the member set are dropped: a Needs entry carrying a
// non-blocking type prefix (e.g. "tracks:x", see internal/molecule
// stepToBead) never resolves to a real member ID, so it silently
// contributes no edge instead of needing separate type filtering — the same
// property runproj.snapshotDeps relies on for its own dependency-edge fold.
func runStepDependencyEdges(members []beads.Bead) map[string][]string {
	memberIDs := make(map[string]bool, len(members))
	for i := range members {
		memberIDs[members[i].ID] = true
	}
	edges := make(map[string][]string, len(members))
	seen := make(map[string]bool)
	add := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		if !memberIDs[from] || !memberIDs[to] {
			return
		}
		key := from + "\x00" + to
		if seen[key] {
			return
		}
		seen[key] = true
		edges[from] = append(edges[from], to)
	}
	for i := range members {
		b := members[i]
		for _, d := range b.Dependencies {
			add(d.DependsOnID, d.IssueID)
		}
		for _, need := range b.Needs {
			add(need, b.ID)
		}
	}
	return edges
}

// topoSortRunSteps orders members into a topological order of their
// step-ordering dependency edges (runStepDependencyEdges), so the Runs API
// returns a run's steps in an order readable as a pipeline instead of
// whatever order the projection fold happened to yield (gascity#4699). Ties
// — independent steps, or steps with no dependency data at all — break on
// bead ID, giving a fully deterministic order across repeated calls. A
// cycle (which a valid formula-produced graph should never contain) cannot
// stall the sort: any bead the frontier never reaches is appended,
// ID-sorted, once the frontier empties, so the result always holds exactly
// len(members) beads.
func topoSortRunSteps(members []beads.Bead) []beads.Bead {
	if len(members) < 2 {
		return members
	}
	byID := make(map[string]beads.Bead, len(members))
	inDegree := make(map[string]int, len(members))
	for i := range members {
		byID[members[i].ID] = members[i]
		inDegree[members[i].ID] = 0
	}
	edges := runStepDependencyEdges(members)
	for _, tos := range edges {
		for _, to := range tos {
			inDegree[to]++
		}
	}

	var frontier []string
	for id, deg := range inDegree {
		if deg == 0 {
			frontier = append(frontier, id)
		}
	}
	sort.Strings(frontier)

	ordered := make([]beads.Bead, 0, len(members))
	visited := make(map[string]bool, len(members))
	for len(frontier) > 0 {
		id := frontier[0]
		frontier = frontier[1:]
		visited[id] = true
		ordered = append(ordered, byID[id])

		var newlyReady []string
		for _, to := range edges[id] {
			inDegree[to]--
			if inDegree[to] == 0 {
				newlyReady = append(newlyReady, to)
			}
		}
		if len(newlyReady) > 0 {
			frontier = append(frontier, newlyReady...)
			sort.Strings(frontier)
		}
	}

	if len(ordered) < len(members) {
		var leftover []string
		for i := range members {
			if !visited[members[i].ID] {
				leftover = append(leftover, members[i].ID)
			}
		}
		sort.Strings(leftover)
		for _, id := range leftover {
			ordered = append(ordered, byID[id])
		}
	}
	return ordered
}
