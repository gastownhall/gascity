//go:build gascity_native_beads

package beads

import "fmt"

var _ BatchGetter = (*DoltliteReadStore)(nil)

// GetBatch retrieves the same SQLite-backed rows as repeated Get calls without
// falling through to the promoted BdStore subprocess implementation. Durable
// issues are hydrated first; only IDs absent there are hydrated from wisps,
// preserving Doltlite Get's issues-first cross-tier authority.
func (s *DoltliteReadStore) GetBatch(ids []string) ([]Bead, error) {
	unique, err := uniqueBatchGetIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("getting doltlite beads batch: %w", err)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	query := ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth}
	rows, err := s.hydrateBeadsByIDStrict(query, []doltliteTableSet{doltliteIssueTables}, unique)
	if err != nil {
		return nil, fmt.Errorf("getting doltlite issue batch: %w", err)
	}

	found := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		found[row.ID] = struct{}{}
	}
	missing := make([]string, 0, len(unique)-len(found))
	for _, id := range unique {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		wisps, wispErr := s.hydrateBeadsByIDStrict(query, []doltliteTableSet{doltliteWispTables}, missing)
		if wispErr != nil {
			return nil, fmt.Errorf("getting doltlite wisp batch: %w", wispErr)
		}
		rows = append(rows, wisps...)
	}

	ordered, err := validateBatchGetRows(unique, rows)
	if err != nil {
		return nil, fmt.Errorf("getting doltlite beads batch: %w", err)
	}
	return ordered, nil
}
