// Package closeorder computes blocker-first close batches for bead stores.
package closeorder

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// Order returns ids reordered so that, for any "blocks" edge whose blocker and
// blocked bead are both in ids, the blocker appears first. Input order is the
// priority among nodes that are not constrained relative to each other. Cycles
// or otherwise unresolvable nodes are appended in input order so the close
// cascade never deadlocks.
func Order(store beads.Store, ids []string) ([]string, error) {
	if store == nil || len(ids) <= 1 {
		return ids, nil
	}

	inSet := make(map[string]bool, len(ids))
	priority := make(map[string]int, len(ids))
	for i, id := range ids {
		inSet[id] = true
		priority[id] = i
	}

	blockedBy := make(map[string]map[string]struct{}, len(ids))
	for _, id := range ids {
		deps, err := store.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing close dependencies for %q: %w", id, err)
		}
		for _, d := range deps {
			if d.IssueID != id || d.Type != "blocks" {
				continue
			}
			if !inSet[d.DependsOnID] || d.DependsOnID == id {
				continue
			}
			if blockedBy[id] == nil {
				blockedBy[id] = make(map[string]struct{})
			}
			blockedBy[id][d.DependsOnID] = struct{}{}
		}
	}

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
	return out, nil
}
