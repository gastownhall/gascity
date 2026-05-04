package api

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

type cachedListStore interface {
	CachedList(beads.ListQuery) ([]beads.Bead, bool)
}

func listSessionBeadsForReadModel(store beads.Store) ([]beads.Bead, error) {
	query := beads.ListQuery{
		Label: session.LabelSession,
		Sort:  beads.SortCreatedDesc,
	}
	if cached, ok := store.(cachedListStore); ok {
		if rows, cacheOK := cached.CachedList(query); cacheOK {
			return rows, nil
		}
	}
	return store.List(query)
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
