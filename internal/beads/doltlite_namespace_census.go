//go:build gascity_native_beads

package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var _ NamespaceCensus = (*DoltliteReadStore)(nil)

// HasResidentOutside reports whether the store holds any bead whose id none of
// prefixes claims. It satisfies NamespaceCensus.
//
// Both storage tables are asked, because both are places a carried-across row
// can sit and TierBoth applies no discriminator to either — the tier and
// storage-flag predicates List layers on for the narrower modes are the ones
// TierBoth deliberately omits. Neither the cross-table dedupe nor the
// storage-flag classification changes the answer: a row that exists in either
// table is a resident whichever tier it is classified into, and a wisp shadowed
// by its durable twin shares that twin's id.
//
// The wisps table is optional — snapshots written before the upstream wisps
// migration have none — so its absence is a table with no rows in it, not a
// failed read.
func (s *DoltliteReadStore) HasResidentOutside(prefixes []string) (bool, error) {
	where, args := namespaceExclusionSQL("COALESCE(i.id, '')", prefixes)
	for _, tables := range doltliteTableSetsForMode(TierBoth) {
		if tables.wisps && !s.tableExists(tables.issues) {
			continue
		}
		query := "SELECT 1 FROM " + tables.issues + " i"
		if where != "" {
			query += " WHERE " + where
		}
		query += " LIMIT 1"

		var found int
		switch err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&found); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return false, fmt.Errorf("censusing the id namespaces of doltlite table %s: %w", tables.issues, err)
		default:
			return true, nil
		}
	}
	return false, nil
}
