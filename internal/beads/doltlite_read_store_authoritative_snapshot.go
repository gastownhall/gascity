//go:build gascity_native_beads

package beads

import "fmt"

var _ AuthoritativeSnapshotter = (*DoltliteReadStore)(nil)

// AuthoritativeSnapshot uses DoltLite's issues-first all-tier query, the same
// query path as point Get, and fails on any SQLite scan or metadata decode
// error. queryIssues already suppresses a wisps-table twin after an issues row.
func (s *DoltliteReadStore) AuthoritativeSnapshot() ([]Bead, error) {
	rows, err := s.queryIssues(ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		TierMode:      TierBoth,
	}, "", nil, 0)
	if err != nil {
		return nil, fmt.Errorf("reading doltlite authoritative snapshot: %w", err)
	}
	return validateAuthoritativeSnapshotRows(rows)
}
