package beads

var _ AuthoritativeSnapshotter = (*MemStore)(nil)

// AuthoritativeSnapshot clones every first-ID row while holding one MemStore
// lock. MemStore.Get uses the first physical row for an ID, so collapsing a
// malformed duplicate fixture the same way preserves exact point authority.
func (m *MemStore) AuthoritativeSnapshot() ([]Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows := make([]Bead, 0, len(m.beads))
	seen := make(map[string]struct{}, len(m.beads))
	for _, row := range m.beads {
		if _, duplicate := seen[row.ID]; duplicate {
			continue
		}
		seen[row.ID] = struct{}{}
		rows = append(rows, cloneBead(row))
	}
	return validateAuthoritativeSnapshotRows(rows)
}
