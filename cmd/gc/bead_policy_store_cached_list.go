package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
)

var (
	_ beads.RefreshingCachedLister = (*beadPolicyStore)(nil)
	_ beads.RefreshingCachedLister = (*beadPolicyGraphStore)(nil)
)

// CachedListRefreshing answers query from the bead cache under this layer,
// at this layer's read tier, as List reads it, once the cache has refreshed
// its dirty rows, waiting for that no longer than ctx allows
// (beads.CachingStore.CachedListRefreshing). It answers false when the
// wrapped store has no cache. The API's list active-work lookup reads the
// controller's city and rig caches through it. The forward is explicit for
// the reason Stats is: the embedded Store interface does not promote optional
// capabilities.
//
// CachedList is deliberately not forwarded. Callers that assert CachedList on
// this layer (the session list's cache-first tier, the reconciler's wisp
// probe) have never reached the cache through it, so forwarding it would
// change their reads too. beadPolicyGraphStore inherits this through its
// embedded *beadPolicyStore.
func (s *beadPolicyStore) CachedListRefreshing(ctx context.Context, query beads.ListQuery) ([]beads.Bead, bool) {
	cache, ok := beads.As[beads.RefreshingCachedLister](s.Store)
	if !ok {
		return nil, false
	}
	return cache.CachedListRefreshing(ctx, expandPolicyReadTier(query))
}
