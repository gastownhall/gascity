package api

import (
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

type cachedListStore interface {
	CachedList(beads.ListQuery) ([]beads.Bead, bool)
}

func listSessionBeadsForReadModel(store beads.Store) ([]beads.Bead, error) {
	// Fast path: ask the cache for both the type and label query shapes
	// the underlying helper will issue, and merge them locally if both
	// hit. This preserves the read-model's cache-first behavior while
	// still picking up canonical beads that lost their gc:session label.
	if cached, ok := store.(cachedListStore); ok {
		typeQuery := beads.ListQuery{Type: session.BeadType, Sort: beads.SortCreatedDesc}
		labelQuery := beads.ListQuery{Label: session.LabelSession, Sort: beads.SortCreatedDesc}
		typeRows, typeOK := cached.CachedList(typeQuery)
		labelRows, labelOK := cached.CachedList(labelQuery)
		if typeOK && labelOK {
			seen := make(map[string]struct{}, len(typeRows)+len(labelRows))
			merged := make([]beads.Bead, 0, len(typeRows)+len(labelRows))
			for _, b := range typeRows {
				if _, dup := seen[b.ID]; dup {
					continue
				}
				if !session.IsSessionBeadOrRepairable(b) {
					continue
				}
				seen[b.ID] = struct{}{}
				merged = append(merged, b)
			}
			for _, b := range labelRows {
				if _, dup := seen[b.ID]; dup {
					continue
				}
				if !session.IsSessionBeadOrRepairable(b) {
					continue
				}
				seen[b.ID] = struct{}{}
				merged = append(merged, b)
			}
			// Match the helper's global sort — the query is hardcoded
			// to SortCreatedDesc, so cached and uncached paths must
			// agree on order across mixed-shape rows.
			sort.SliceStable(merged, func(i, j int) bool {
				return merged[i].CreatedAt.After(merged[j].CreatedAt)
			})
			return merged, nil
		}
	}
	return session.ListAllSessionBeads(store, beads.ListQuery{Sort: beads.SortCreatedDesc})
}

func sessionReadModelRows(store beads.Store) ([]beads.Bead, []string, error) {
	rows, err := listSessionBeadsForReadModel(store)
	if err == nil {
		return rows, nil, nil
	}
	if beads.IsPartialResult(err) && len(rows) > 0 {
		return rows, []string{err.Error()}, nil
	}
	return nil, nil, err
}
