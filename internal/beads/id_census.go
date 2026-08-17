package beads

import (
	"fmt"
	"strings"
)

// CompleteIDScanner is an optional Store capability for enumerating every ID
// in that store's local all-tier corpus across all statuses. It exists for
// fail-closed recovery paths that need a complete identity census without
// depending on List's intentionally tolerant row decoding. Implementations may
// collapse the same physical ID across tiers, but they must not silently omit
// an ID because another field on that row is malformed.
//
// Callers should use ScanAllIDs instead of asserting this interface directly.
type CompleteIDScanner interface {
	ScanAllIDs() ([]string, error)
}

// ScanAllIDs returns every ID visible to the store across open and closed rows
// and both durable and wisp tiers. Stores without CompleteIDScanner fall back
// to an unbounded List. Any scan error or malformed ID fails the whole census;
// duplicate IDs are collapsed in stable first-seen order.
func ScanAllIDs(store Store) ([]string, error) {
	if store == nil {
		return nil, fmt.Errorf("scanning all bead IDs: nil store")
	}

	var (
		ids []string
		err error
	)
	if scanner, ok := store.(CompleteIDScanner); ok {
		ids, err = scanner.ScanAllIDs()
	} else {
		var rows []Bead
		rows, err = store.List(ListQuery{
			AllowScan:     true,
			IncludeClosed: true,
			SkipLabels:    true,
			TierMode:      TierBoth,
		})
		if err == nil {
			ids = make([]string, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("scanning all bead IDs: %w", err)
	}

	ids, err = stableUniqueCensusIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("scanning all bead IDs: %w", err)
	}
	return ids, nil
}

func stableUniqueCensusIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for i, id := range ids {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("malformed bead ID at index %d", i)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique, nil
}
