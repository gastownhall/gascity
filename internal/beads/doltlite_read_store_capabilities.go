//go:build gascity_native_beads

package beads

// UpdateReporting updates through the embedded BdStore and resets the
// order-run cache exactly as Update does, so the capability promoted from
// BdStore cannot skip the reset.
func (s *DoltliteReadStore) UpdateReporting(id string, opts UpdateOpts) (UpdatedRow, error) {
	row, err := s.BdStore.UpdateReporting(id, opts)
	if err == nil {
		s.resetOrderRunCache()
	}
	return row, err
}

// ChildrenOfAny reads each parent's children through this store's own
// in-process Children, so the batched read promoted from BdStore cannot route
// around it.
func (s *DoltliteReadStore) ChildrenOfAny(parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
	return childrenOfAnyPerParent(s, parentIDs, opts...)
}
