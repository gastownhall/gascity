package api

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// cachedListStore is the optional read-model cache capability: a store that can
// answer a ListQuery from its in-memory cache, reporting whether the cache was
// clean enough to serve it. The session read model now peeks this seam inside
// session.Store.ListAll; other read-model consumers (agents, orders) still assert
// it directly on the raw store.
type cachedListStore interface {
	CachedList(beads.ListQuery) ([]beads.Bead, bool)
}

// sessionReadModelListings is the typed read-model feed: the cache-first union
// (session.Store.ListAllWithResponses) projected to (Info, PersistedResponse)
// rows, wrapped in the same partial-result envelope as sessionReadModelRows. It
// is the typed twin of sessionReadModelRows — the pre-joined pair per row means
// the response builder never needs a bead index to re-attach the persisted
// projection. The cache-first tier (#3939/#3941) is preserved inside
// Store.ListAll: a warm cachedListStore serves the whole list with zero
// store.List calls (pinned by TestSessionReadModelListingsWarmCacheZeroStoreList).
func sessionReadModelListings(sessFront *session.Store) ([]session.ListedSession, []string, error) {
	rows, err := sessFront.ListAllWithResponses(session.ListAllOptions{
		Sort:       beads.SortCreatedDesc,
		CacheFirst: true,
	})
	if err == nil {
		return rows, nil, nil
	}
	if beads.IsPartialResult(err) && len(rows) > 0 {
		return rows, []string{err.Error()}, nil
	}
	return nil, nil, err
}

// filterEnrichReadModel filters a typed read-model feed by state and template and
// applies the runtime overlay (Manager.ListFromInfos), returning the enriched
// session list paired with a by-id lookup of each session's persisted-response
// projection. The pair is pre-joined per ListedSession row, so the session
// response builder re-attaches the persisted facts by id — no bead index and no
// bead->response projection. Filter-then-enrich order is preserved inside
// ListFromInfos (the persisted state filter runs before the runtime downgrade).
func filterEnrichReadModel(mgr *session.Manager, listings []session.ListedSession, stateFilter, templateFilter string) ([]session.Info, map[string]session.PersistedResponse) {
	infos := make([]session.Info, len(listings))
	responseByID := make(map[string]session.PersistedResponse, len(listings))
	for i, listing := range listings {
		infos[i] = listing.Info
		responseByID[listing.Info.ID] = listing.Response
	}
	return mgr.ListFromInfos(infos, stateFilter, templateFilter), responseByID
}

// sessionReadModelInfos is the Info-only variant of sessionReadModelListings for
// read-model consumers that do not need the persisted-response projection (the
// status snapshot and the city-pending aggregate filter and probe by Info alone).
// Same cache-first tier and partial-result envelope.
func sessionReadModelInfos(sessFront *session.Store) ([]session.Info, []string, error) {
	rows, err := sessFront.ListAll(session.ListAllOptions{
		Sort:       beads.SortCreatedDesc,
		CacheFirst: true,
	})
	if err == nil {
		return rows, nil, nil
	}
	if beads.IsPartialResult(err) && len(rows) > 0 {
		return rows, []string{err.Error()}, nil
	}
	return nil, nil, err
}

// sessionReadModelInfosContext is sessionReadModelInfos but accepts a context so
// a caller with a deadline (the status endpoint's session snapshot) can cancel
// the backing queries — via beads.ContextLister when the store supports it —
// instead of leaking a goroutine on timeout. Same cache-first tier and
// partial-result envelope; the cache-hit path is in-memory and ctx-independent.
func sessionReadModelInfosContext(ctx context.Context, sessFront *session.Store) ([]session.Info, []string, error) {
	rows, err := sessFront.ListAllContext(ctx, session.ListAllOptions{
		Sort:       beads.SortCreatedDesc,
		CacheFirst: true,
	})
	if err == nil {
		return rows, nil, nil
	}
	if beads.IsPartialResult(err) && len(rows) > 0 {
		return rows, []string{err.Error()}, nil
	}
	return nil, nil, err
}
