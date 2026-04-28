package molecule

import (
	"cmp"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// ListSubtree returns the root bead and all transitive parent-child
// descendants, including already-closed beads so nested open descendants are
// still reachable through a closed intermediate node.
func ListSubtree(store beads.Store, rootID string) ([]beads.Bead, error) {
	rootID = strings.TrimSpace(rootID)
	if store == nil || rootID == "" {
		return nil, nil
	}
	root, err := store.Get(rootID)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{root.ID: {}}
	out := []beads.Bead{root}
	queue := []string{root.ID}

	logicalMembers, err := store.ListByMetadata(map[string]string{"gc.root_bead_id": root.ID}, 0, beads.IncludeClosed)
	if err != nil {
		return nil, err
	}
	for _, bead := range logicalMembers {
		if bead.ID == "" {
			continue
		}
		if _, ok := seen[bead.ID]; ok {
			continue
		}
		seen[bead.ID] = struct{}{}
		out = append(out, bead)
		queue = append(queue, bead.ID)
	}

	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]

		children, err := store.Children(parentID, beads.IncludeClosed)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child.ID == "" {
				continue
			}
			if _, ok := seen[child.ID]; ok {
				continue
			}
			seen[child.ID] = struct{}{}
			out = append(out, child)
			queue = append(queue, child.ID)
		}
	}
	return out, nil
}

// CloseSubtree closes the root bead and every open descendant.
// Descendants are closed before the root so stores with stricter
// parent/child close rules can still accept the operation. Within the
// open set, closes are emitted in topological order honoring "blocks"
// dependency edges between subtree members (blockers first), so that
// the bd close validator does not reject a bead while its in-batch
// blocker is still open. Parent/child depth (deepest first) is used as
// the tie-breaker when no blocks edge constrains the order.
func CloseSubtree(store beads.Store, rootID string) (int, error) {
	matched, err := ListSubtree(store, rootID)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]beads.Bead, len(matched))
	for _, bead := range matched {
		byID[bead.ID] = bead
	}
	depthMemo := make(map[string]int, len(matched))
	const visitingDepth = -1
	var depth func(string) int
	depth = func(id string) int {
		if d, ok := depthMemo[id]; ok {
			if d == visitingDepth {
				return 0
			}
			return d
		}
		bead, ok := byID[id]
		if !ok {
			return 0
		}
		parentID := strings.TrimSpace(bead.ParentID)
		if parentID == "" || parentID == id {
			depthMemo[id] = 0
			return 0
		}
		parent, ok := byID[parentID]
		if !ok || parent.ID == "" {
			depthMemo[id] = 0
			return 0
		}
		depthMemo[id] = visitingDepth
		d := depth(parentID) + 1
		depthMemo[id] = d
		return d
	}
	slices.SortFunc(matched, func(a, b beads.Bead) int {
		if da, db := depth(a.ID), depth(b.ID); da != db {
			return cmp.Compare(db, da)
		}
		return cmp.Compare(a.ID, b.ID)
	})

	ids := make([]string, 0, len(matched))
	for _, bead := range matched {
		if bead.ID == "" || bead.Status == "closed" {
			continue
		}
		ids = append(ids, bead.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ordered := orderForClose(store, ids)
	return store.CloseAll(ordered, nil)
}

// orderForClose returns ids reordered so that, for any "blocks" edge
// whose blocker and blocked are both in ids, the blocker appears
// first. Input order is treated as the priority among nodes that are
// not constrained relative to each other (preserving the depth-then-id
// tie-break the caller computed). Cycles or unresolvable nodes are
// appended in input order so the cascade never deadlocks.
func orderForClose(store beads.Store, ids []string) []string {
	if len(ids) <= 1 {
		return ids
	}
	inSet := make(map[string]bool, len(ids))
	priority := make(map[string]int, len(ids))
	for i, id := range ids {
		inSet[id] = true
		priority[id] = i
	}
	// blockedBy[x] = ids in the batch that x must wait for (blockers).
	// blocks[x]    = ids in the batch that x is a blocker for.
	blockedBy := make(map[string]map[string]struct{}, len(ids))
	blocks := make(map[string]map[string]struct{}, len(ids))
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			continue
		}
		for _, d := range deps {
			if d.IssueID != id || d.Type != "blocks" {
				continue
			}
			if !inSet[d.DependsOnID] {
				continue
			}
			if d.DependsOnID == id {
				continue
			}
			if blockedBy[id] == nil {
				blockedBy[id] = make(map[string]struct{})
			}
			blockedBy[id][d.DependsOnID] = struct{}{}
			if blocks[d.DependsOnID] == nil {
				blocks[d.DependsOnID] = make(map[string]struct{})
			}
			blocks[d.DependsOnID][id] = struct{}{}
		}
	}
	// Kahn's algorithm: repeatedly emit a node with no remaining
	// in-batch blockers, breaking ties on the caller-supplied priority
	// (depth-descending, id-ascending).
	out := make([]string, 0, len(ids))
	emitted := make(map[string]bool, len(ids))
	for len(out) < len(ids) {
		var pick string
		pickPrio := -1
		for _, id := range ids {
			if emitted[id] {
				continue
			}
			ready := true
			for blocker := range blockedBy[id] {
				if !emitted[blocker] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			if pick == "" || priority[id] < pickPrio {
				pick = id
				pickPrio = priority[id]
			}
		}
		if pick == "" {
			// Cycle (or unresolvable graph): append remaining nodes in
			// input order. CloseAll will still try them; the bd
			// validator may reject some, but we never deadlock here.
			for _, id := range ids {
				if !emitted[id] {
					out = append(out, id)
					emitted[id] = true
				}
			}
			break
		}
		out = append(out, pick)
		emitted[pick] = true
	}
	return out
}
