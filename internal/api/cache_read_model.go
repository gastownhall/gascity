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
