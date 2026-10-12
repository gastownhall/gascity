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
	return childrenOfAnyPerParent(store, parentIDs, opts...)
}

// childrenOfAnyPerParent is the per-parent form of ChildrenOfAny: one Children
// read per parent, children deduplicated in first-seen order.
func childrenOfAnyPerParent(store Store, parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
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
// `bd dep list <ids> --direction=up --type=parent-child` and then reads the
// non-ephemeral ones in full with one exact batched `bd show`, two bd calls in
// place of one `bd list --parent` per parent.
//
// It answers exactly what Children(id, IncludeClosed) answers per parent on
// this store, which ignores tier options: `bd list` never surfaces ephemeral
// rows, so ephemeral children the dependency read reports are dropped, and the
// `bd show` rows carry the ParentID and CreatedAt a caller orders by.
func (s *BdStore) ChildrenOfAny(parentIDs []string, _ ...QueryOpt) ([]Bead, error) {
	if len(parentIDs) == 0 {
		return nil, nil
	}
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
		if row.ID == "" || row.Ephemeral || listed[row.ID] {
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
		if bead, ok := found[id]; ok {
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
		children = append(children, bead)
	}
	return children, nil
}
