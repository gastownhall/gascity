package beads

import (
	"fmt"
	"sort"
	"strings"
)

// AuthoritativeSnapshotter is an optional Store capability for reading one
// complete, all-status/all-tier recovery projection. For every ID, the chosen
// tier and the ID, Status, and Metadata fields must match the row the store's
// point Get would return. Fields outside that lifecycle-recovery contract (for
// example embedded dependency or label collections) may be incomplete when a
// backend's bulk-list surface does not hydrate them. Implementations must fail
// rather than return a partial projection when either backing tier cannot be
// read or a row cannot be decoded.
//
// The capability exists for fail-closed recovery passes that need complete
// lifecycle projections.
// It avoids the otherwise necessary ScanAllIDs followed by GetBatch over the
// entire corpus while retaining point-Get authority across tier collisions.
type AuthoritativeSnapshotter interface {
	AuthoritativeSnapshot() ([]Bead, error)
}

// AuthoritativeSnapshot returns one complete, deterministic, ID-sorted
// recovery projection. Stores without the optional capability retain the strict
// ScanAllIDs+GetBatch fallback. A malformed or duplicate result fails the
// whole read; callers must not act on partial rows.
func AuthoritativeSnapshot(store Store) ([]Bead, error) {
	if store == nil {
		return nil, fmt.Errorf("reading authoritative bead snapshot: nil store")
	}

	var (
		rows []Bead
		err  error
	)
	if snapshotter, ok := store.(AuthoritativeSnapshotter); ok {
		rows, err = snapshotter.AuthoritativeSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading authoritative bead snapshot: %w", err)
		}
	} else {
		var ids []string
		ids, err = ScanAllIDs(store)
		if err != nil {
			return nil, fmt.Errorf("reading authoritative bead snapshot: %w", err)
		}
		sort.Strings(ids)
		rows, err = GetBatch(store, ids)
		if err != nil {
			return nil, fmt.Errorf("reading authoritative bead snapshot: %w", err)
		}
	}

	rows, err = validateAuthoritativeSnapshotRows(rows)
	if err != nil {
		return nil, fmt.Errorf("reading authoritative bead snapshot: %w", err)
	}
	return rows, nil
}

func validateAuthoritativeSnapshotRows(rows []Bead) ([]Bead, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(rows))
	for i, row := range rows {
		if strings.TrimSpace(row.ID) == "" {
			return nil, fmt.Errorf("malformed snapshot row at index %d: empty bead ID", i)
		}
		if _, duplicate := seen[row.ID]; duplicate {
			return nil, fmt.Errorf("duplicate bead %q in authoritative snapshot", row.ID)
		}
		seen[row.ID] = struct{}{}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}
