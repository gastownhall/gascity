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

// CloseSubtree closes the root bead and every open descendant. Descendants are
// closed before the root so stores with stricter parent/child close rules can
// still accept the operation.
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
	return store.CloseAll(ids, nil)
}
