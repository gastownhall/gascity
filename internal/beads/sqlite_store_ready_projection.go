package beads

import (
	"context"
	"fmt"
	"strings"
)

// A CachingStore over SQLiteStore derives readiness itself: fetchDepsForIDs
// reports complete deps, so the cache serves Ready from its resident rows.
// Without this column it falls back to the dependency-derived predicate, which
// ignores an edge onto a row the cache does not hold — a missing or foreign
// blocker, or a closed blocker carrying gc.work_outcome=blocked — all of which
// block in sqliteReadySQL. Losing this method silently reopens that gap.
var _ readyProjectionEnrichmentStore = (*SQLiteStore)(nil)

// sqliteReadyProjectionBatch bounds how many ids ride on one projection query,
// staying well under SQLite's bound-parameter limit.
const sqliteReadyProjectionBatch = 256

// enrichReadyProjectionForCache stamps IsBlocked on every non-closed item with
// the blocking predicate sqliteReadySQL negates (sqliteReadyBlockerExists), so
// a cache over this store answers readiness exactly as Ready does.
//
// Status, type, tier, deferral and labels are not part of the verdict: the
// cache applies those per read through IsReadyCandidateForTier, as readyRows
// does after decode. An id the store no longer holds keeps its current value,
// matching the bd and native projections: a row that raced out of the store is
// not given an invented verdict.
func (s *SQLiteStore) enrichReadyProjectionForCache(items []Bead) ([]Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return items, err
	}
	ids := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item.ID == "" || item.Status == "closed" {
			continue
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		ids = append(ids, item.ID)
	}
	if len(ids) == 0 {
		return items, nil
	}

	blocked := make(map[string]bool, len(ids))
	for start := 0; start < len(ids); start += sqliteReadyProjectionBatch {
		end := min(start+sqliteReadyProjectionBatch, len(ids))
		if err := s.readBlockedBatch(ids[start:end], blocked); err != nil {
			return items, err
		}
	}

	enriched := make([]Bead, len(items))
	copy(enriched, items)
	for i := range enriched {
		if enriched[i].Status == "closed" {
			continue
		}
		verdict, ok := blocked[enriched[i].ID]
		if !ok {
			continue
		}
		enriched[i].IsBlocked = cloneBoolPtr(&verdict)
	}
	return enriched, nil
}

// readBlockedBatch records in into the blocking verdict of each id in chunk
// that the store holds.
func (s *SQLiteStore) readBlockedBatch(chunk []string, into map[string]bool) error {
	args := make([]any, len(chunk))
	placeholders := make([]string, len(chunk))
	for i, id := range chunk {
		args[i] = id
		placeholders[i] = "?"
	}
	rows, err := s.readDB.QueryContext(context.Background(),
		`SELECT b.id, `+sqliteReadyBlockerExists("b.id")+` FROM beads b WHERE b.id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("sqlite ready projection: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var id string
		var isBlocked bool
		if err := rows.Scan(&id, &isBlocked); err != nil {
			return fmt.Errorf("sqlite ready projection: %w", err)
		}
		into[id] = isBlocked
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite ready projection: %w", err)
	}
	return nil
}
