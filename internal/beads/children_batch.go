package beads

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ChildrenBatchLister is the optional capability of a store that can read the
// parent-child children of many parents in one round trip. A breadth-first
// walk over a molecule's parent-child closure otherwise asks once per node.
//
// ChildrenOfAny returns the union of Children(id, IncludeClosed, opts...)
// over parentIDs: closed children included, each child once, in no particular
// order. Callers that need an order impose it from the rows' ParentID and
// CreatedAt.
type ChildrenBatchLister interface {
	ChildrenOfAny(parentIDs []string, opts ...QueryOpt) ([]Bead, error)
}

// ChildrenOfAny returns the union of Children(id, IncludeClosed, opts...)
// over parentIDs, through the store's batched read when it has one and one
// Children call per parent otherwise.
func ChildrenOfAny(store Store, parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	if batch, ok := store.(ChildrenBatchLister); ok {
		return batch.ChildrenOfAny(parentIDs, opts...)
	}
	return ChildrenOfAnyPerParent(store, parentIDs, opts...)
}

// ChildrenOfAnyPerParent is the per-parent form of ChildrenOfAny: one Children
// read per parent, children deduplicated in first-seen order. A wrapper that
// forwards ChildrenBatchLister answers with it when its inner store has no
// batched read, so the wrapper's own Children rules still apply.
func ChildrenOfAnyPerParent(store Store, parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
	var out []Bead
	seen := make(map[string]bool)
	for _, parentID := range parentIDs {
		children, err := store.Children(parentID, append([]QueryOpt{IncludeClosed}, opts...)...)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child.ID == "" || seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			out = append(out, child)
		}
	}
	return out, nil
}

var _ ChildrenBatchLister = (*BdStore)(nil)

// ChildrenOfAny reads the children of every parent with one
// `bd dep list <ids> --direction=up --type=parent-child` and then reads them in
// full with one exact batched `bd show`: two bd calls in place of one
// `bd list --parent` per parent, plus one `bd query` per parent for a read
// that includes the wisp tier.
//
// The tier options select rows as ListQuery.TierMode does for a List over
// ParentID. With none, it answers what Children(id, IncludeClosed) answers per
// parent on this store: `bd list` never surfaces ephemeral rows, so ephemeral
// children the dependency read reports are dropped. WithBothTiers keeps them,
// as List{ParentID, TierMode: TierBoth} does by unioning `bd list --parent`
// with `bd query "ephemeral=true AND parent=..."`. The bead-policy layer reads
// children at TierBoth, so the batch it forwards must keep them too. The
// dependency read reports wisp children because bd keeps their parent-child
// edges in wisp_dependencies, which `bd dep list` reads; their rows come from
// the same `bd show`, or from Get's wisp fallback when a bd's show does not
// answer them. The `bd show` rows carry the ParentID and CreatedAt a caller
// orders by.
func (s *BdStore) ChildrenOfAny(parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
	tier := ListQuery{TierMode: TierModeFromOpts(opts)}
	args := append([]string{"dep", "list"}, parentIDs...)
	args = append(args, "--direction=up", "--type=parent-child", "--json")
	out, err := s.runBDTransientRead(args...)
	if err != nil {
		if isBdNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("batch children: %w", err)
	}
	extracted := extractJSON(out)
	if len(extracted) == 0 || string(extracted) == "[]" {
		return nil, nil
	}
	var rows []struct {
		ID        string `json:"id"`
		Ephemeral bool   `json:"ephemeral"`
	}
	if err := json.Unmarshal(extracted, &rows); err != nil {
		return nil, fmt.Errorf("batch children: parsing JSON: %w", err)
	}
	var ids []string
	listed := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.ID == "" || listed[row.ID] || (row.Ephemeral && tier.TierMode == TierIssues) {
			continue
		}
		listed[row.ID] = true
		ids = append(ids, row.ID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	found, unresolved, err := s.GetExactBatch(ids)
	if err != nil {
		return nil, fmt.Errorf("batch children: %w", err)
	}
	children := make([]Bead, 0, len(ids))
	for _, id := range ids {
		if bead, ok := found[id]; ok && tier.matchesTier(bead) {
			children = append(children, bead)
		}
	}
	for _, id := range unresolved {
		bead, err := s.Get(id)
		if errors.Is(err, ErrNotFound) {
			// Gone between the two reads: Children read after the delete
			// would not list it either.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("batch children: %w", err)
		}
		if tier.matchesTier(bead) {
			children = append(children, bead)
		}
	}
	return children, nil
}
