package main

import (
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionListCache wraps a beads.Store and memoizes the canonical
// session-label list query for the lifetime of a single command
// invocation. The status renderer resolves 25+ agents, each of which
// triggers session.ResolveSessionID + loadSessionBeadSnapshot. Without
// memoization every agent issues an identical Label="gc:session" list
// scan against Dolt, costing ~500-900ms per call. The wrapper collapses
// all of those into a single backing fetch.
//
// The wrapper only intercepts queries whose only selector is the
// session label (with optional IncludeClosed + Sort). Any query that
// adds a Status, Type, Assignee, ParentID, Metadata, CreatedBefore, or
// AllowScan filter — or that sets Live to bypass caching — is passed
// straight through.
type sessionListCache struct {
	beads.Store
	once   sync.Once
	closed []beads.Bead
	err    error
}

// wrapSessionListCache returns a Store that memoizes the canonical
// session-label list query. A nil store passes through.
func wrapSessionListCache(s beads.Store) beads.Store {
	if s == nil {
		return nil
	}
	return &sessionListCache{Store: s}
}

// List intercepts canonical session-label queries and serves them from
// a single backing fetch; other queries delegate to the wrapped store.
func (c *sessionListCache) List(q beads.ListQuery) ([]beads.Bead, error) {
	if !isCanonicalSessionListQuery(q) {
		return c.Store.List(q)
	}
	c.once.Do(func() {
		c.closed, c.err = c.Store.List(beads.ListQuery{
			Label:         session.LabelSession,
			IncludeClosed: true,
		})
	})
	if c.err != nil {
		return nil, c.err
	}
	return beads.ApplyListQuery(c.closed, q), nil
}

// isCanonicalSessionListQuery reports whether q is one of the
// session-bead lookup shapes used on the status hot path. Other
// selectors disqualify the cache so we never accidentally serve a
// narrowed query from a wider snapshot.
func isCanonicalSessionListQuery(q beads.ListQuery) bool {
	if q.Live || q.Label != session.LabelSession {
		return false
	}
	if q.Status != "" || q.Type != "" || q.Assignee != "" || q.ParentID != "" {
		return false
	}
	if len(q.Metadata) > 0 || !q.CreatedBefore.IsZero() || q.AllowScan {
		return false
	}
	return true
}
