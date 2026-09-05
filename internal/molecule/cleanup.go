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

// ListSubtree returns the beads.MembershipRootIDAndParentClosure member set of
// the molecule rooted at rootID: the root, every bead carrying
// gc.root_bead_id == rootID, and the transitive parent-child closure of all of
// them. Closed beads are included so a nested open descendant is still
// reachable through a closed intermediate node.
//
// The root-id arm is load-bearing, not decorative. Teardown callers
// (CloseSubtree, sling.CloseAttachedSubtree, formula teardown) must reach the
// dependency-isolated members — gc.kind=spec sidecars carry no dep edges and,
// when materialization set no parent, no parent edge either — or the subtree
// stays half-open and cleanup reports success on an incomplete close.
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
	return CloseSubtreeWithMetadata(store, rootID, map[string]string{
		"close_reason": SubtreeClosedReason,
	})
}

// CloseSubtreeWithMetadata closes the root bead and every open descendant,
// stamping metadata on each newly closed bead. It preserves CloseSubtree's
// descendant-first, blocker-first ordering and is idempotent for an already
// closed subtree.
func CloseSubtreeWithMetadata(store beads.Store, rootID string, metadata map[string]string) (int, error) {
	return CloseSubtreeWithMetadataExcept(store, rootID, metadata, nil)
}

// TeardownTailExclusion builds the predicate that keeps a workflow's teardown
// tail out of a terminal sweep over its subtree. Teardown work runs after the
// root settles by contract (its pass condition may branch on the run
// outcome), so force-closing it at settlement, or at cancellation, would skip
// the very step that releases the workflow's resources.
//
// The tail is the teardown-scoped members plus every attempt of the same step:
// retry expansion strips gc.scope_role from the first attempt, leaving
// gc.step_id as the only durable link back to the teardown step.
func TeardownTailExclusion(store beads.Store, rootID string) (func(beads.Bead) bool, error) {
	members, err := ListSubtree(store, rootID)
	if err != nil {
		return nil, err
	}
	teardownStepIDs := make(map[string]struct{})
	for _, member := range members {
		if member.Metadata[beadmeta.ScopeRoleMetadataKey] != beadmeta.ScopeRoleTeardown {
			continue
		}
		if stepID := strings.TrimSpace(member.Metadata[beadmeta.StepIDMetadataKey]); stepID != "" {
			teardownStepIDs[stepID] = struct{}{}
		}
	}
	return func(member beads.Bead) bool {
		if member.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
			return true
		}
		stepID := strings.TrimSpace(member.Metadata[beadmeta.StepIDMetadataKey])
		if stepID == "" {
			return false
		}
		_, ok := teardownStepIDs[stepID]
		return ok
	}, nil
}

// CloseSubtreeWithMetadataExcept is CloseSubtreeWithMetadata with an exclusion
// predicate: any member for which exclude reports true is left untouched. A nil
// predicate closes the whole subtree.
//
// The exclusion exists for members that stay executable after their molecule
// reaches a terminal state — teardown work, which by contract runs after the
// root settles. Callers own the policy; this function only skips.
func CloseSubtreeWithMetadataExcept(store beads.Store, rootID string, metadata map[string]string, exclude func(beads.Bead) bool) (int, error) {
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
		if exclude != nil && exclude(bead) {
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
	return store.CloseAll(ordered, metadata)
}
