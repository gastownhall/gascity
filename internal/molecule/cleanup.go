package molecule

import (
	"cmp"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/closeorder"
)

// SubtreeClosedReason is the canonical close_reason stamped on every
// bead in a molecule subtree when CloseSubtree force-closes it. Without
// an explicit reason of >=20 chars, bd's validation.on-close=error
// rejects the close, the subtree stays open, and downstream cleanup
// (sling.CloseAttachedSubtree, formula teardown, etc.) is silently
// incomplete.
const SubtreeClosedReason = "molecule cleanup: subtree force-closed by CloseSubtree"

// gateBeadType is the bead Type stamped on async gate beads (human gates and
// other wait conditions; see deferBeadRouting and the formula compiler). An
// open gate paired with an open workflow-finalize in the same subtree is a
// parked resumption handle that CloseSubtree must not force-close.
const gateBeadType = "gate"

// isGateBead reports whether the bead is an async gate bead.
func isGateBead(b beads.Bead) bool {
	return b.Type == gateBeadType
}

// isWorkflowFinalizeBead reports whether the bead is a graph workflow's
// finalize control bead.
func isWorkflowFinalizeBead(b beads.Bead) bool {
	return b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize
}

// subtreeHasParkedGate reports whether the matched subtree holds the live
// gate→finalize park handle: at least one open gate AND at least one open
// workflow-finalize. The finalize is what proves the park is still live; an
// open gate without a pending finalize is an ordinary sibling and closes
// normally, preserving the pre-existing teardown behavior for non-gate
// molecules.
func subtreeHasParkedGate(matched []beads.Bead) bool {
	var openGate, openFinalize bool
	for _, bead := range matched {
		if bead.Status == "closed" {
			continue
		}
		if isGateBead(bead) {
			openGate = true
		}
		if isWorkflowFinalizeBead(bead) {
			openFinalize = true
		}
	}
	return openGate && openFinalize
}

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

	logicalMembers, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, 0, beads.IncludeClosed, beads.WithBothTiers)
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

		children, err := store.Children(parentID, beads.IncludeClosed, beads.WithBothTiers)
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
// dependency edges between subtree members (blockers first), so strict
// stores do not reject a bead while its in-batch blocker is still open.
// Parent/child depth (deepest first) is used as the tie-breaker when no
// blocks edge constrains the order.
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

	// A parked human-gate molecule keeps an open gate bead paired with an open
	// workflow-finalize: the gate is the resumption handle and the finalize is
	// the gate-driven work (merge, etc.) that must run once the human releases
	// the gate. When the dispatch/loop bead closes, the autoclose hook reaches
	// here via sling.CloseAttachedSubtree; force-closing the gate or the
	// finalize would destroy that handle, so the finalize could never run.
	// Detect the pair and preserve exactly those two structural beads while
	// still force-closing every unrelated open sibling.
	preservePark := subtreeHasParkedGate(matched)

	ids := make([]string, 0, len(matched))
	for _, bead := range matched {
		if bead.ID == "" || bead.Status == "closed" {
			continue
		}
		if preservePark && (isGateBead(bead) || isWorkflowFinalizeBead(bead)) {
			continue
		}
		ids = append(ids, bead.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ordered, err := closeorder.Order(store, ids)
	if err != nil {
		return 0, err
	}
	return store.CloseAll(ordered, map[string]string{
		"close_reason": SubtreeClosedReason,
	})
}
