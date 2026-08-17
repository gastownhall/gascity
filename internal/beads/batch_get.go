package beads

import (
	"fmt"
	"strings"
)

// BatchGetter is an optional Store capability for authoritatively reading many
// beads at once. Each returned row must be exactly the row Get would return for
// that ID, including the store's precedence rules when an ID exists in more
// than one backing tier. Implementations collapse duplicate requests and return
// one row per unique ID in stable first-input order. A missing, duplicate,
// unexpected, malformed, or partial result fails the whole batch; best-effort
// rows are not valid.
//
// Callers should use GetBatch instead of asserting this interface directly.
// The helper preserves first-request order, validates capability results, and
// falls back to Store.Get when the concrete store lacks the capability.
type BatchGetter interface {
	GetBatch(ids []string) ([]Bead, error)
}

// GetBatch returns the authoritative Get-equivalent row for each unique ID in
// stable first-input order. Duplicate requested IDs are collapsed. A missing,
// duplicate, unexpected, or malformed result makes the whole batch fail; no
// partial rows are returned. Stores without BatchGetter are read once per
// unique ID through Store.Get with the same validation and ordering contract.
func GetBatch(store Store, ids []string) ([]Bead, error) {
	unique, err := uniqueBatchGetIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("getting beads batch: %w", err)
	}
	if len(unique) == 0 {
		return nil, nil
	}
	if store == nil {
		return nil, fmt.Errorf("getting beads batch: nil store")
	}

	var rows []Bead
	if getter, ok := store.(BatchGetter); ok {
		rows, err = getter.GetBatch(unique)
		if err != nil {
			return nil, fmt.Errorf("getting beads batch: %w", err)
		}
	} else {
		rows = make([]Bead, 0, len(unique))
		for _, id := range unique {
			row, getErr := store.Get(id)
			if getErr != nil {
				return nil, fmt.Errorf("getting beads batch: %w", getErr)
			}
			rows = append(rows, row)
		}
	}

	ordered, err := validateBatchGetRows(unique, rows)
	if err != nil {
		return nil, fmt.Errorf("getting beads batch: %w", err)
	}
	return ordered, nil
}

// uniqueBatchGetIDs validates and stable-deduplicates requested IDs without
// retaining or modifying the caller's slice.
func uniqueBatchGetIDs(ids []string) ([]string, error) {
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

// validateBatchGetRows validates an implementation's all-or-nothing response
// and restores the request's stable order. ids must already be unique.
func validateBatchGetRows(ids []string, rows []Bead) ([]Bead, error) {
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}

	byID := make(map[string]Bead, len(rows))
	for i, row := range rows {
		if strings.TrimSpace(row.ID) == "" {
			return nil, fmt.Errorf("malformed batch result at index %d: empty bead ID", i)
		}
		if _, ok := wanted[row.ID]; !ok {
			return nil, fmt.Errorf("unexpected bead %q in batch result", row.ID)
		}
		if _, exists := byID[row.ID]; exists {
			return nil, fmt.Errorf("duplicate bead %q in batch result", row.ID)
		}
		byID[row.ID] = row
	}

	ordered := make([]Bead, len(ids))
	for i, id := range ids {
		row, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("missing bead %q from batch result: %w", id, ErrNotFound)
		}
		ordered[i] = row
	}
	return ordered, nil
}
