package beads

import (
	"errors"
	"fmt"
	"time"
)

// List returns beads matching the query. Active-bead queries are served from
// cache when available. IncludeClosed queries merge cached active results with
// backing-store history when possible so callers keep the old best-effort
// behavior from ListByLabel/ListByMetadata during transient bd failures.
func (c *CachingStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	if query.Live || query.ParentID != "" {
		c.mu.RLock()
		startSeq := c.mutationSeq
		c.mu.RUnlock()
		items, err := c.backing.List(query)
		if err == nil {
			items = c.refreshCachedBeads(query, startSeq, items)
		}
		return items, err
	}

	c.mu.RLock()
	state := c.state
	if state == cacheLive || state == cachePartial {
		if len(c.dirty) > 0 {
			c.mu.RUnlock()
			return c.backing.List(query)
		}
		// PrimeActive loads the full active set (open + in_progress), so
		// active-only queries are complete even before the history prime finishes.
		cached := make([]Bead, 0, len(c.beads))
		for _, b := range c.beads {
			if !query.Matches(b) {
				continue
			}
			cached = append(cached, cloneBead(b))
		}
		c.mu.RUnlock()

		finish := func(items []Bead) ([]Bead, error) {
			sortBeadsForQuery(items, query.Sort)
			if query.Limit > 0 && len(items) > query.Limit {
				items = items[:query.Limit]
			}
			return items, nil
		}

		if !query.IncludesClosed() {
			return finish(cached)
		}

		// The cache never has a complete closed-only or parent-history view, so
		// preserve the old backing-store behavior for those query shapes.
		if query.Status == "closed" || query.ParentID != "" {
			return c.backing.List(query)
		}

		all, err := c.backing.List(query)
		if err != nil {
			return finish(cached)
		}

		seen := make(map[string]bool, len(cached))
		for _, b := range cached {
			seen[b.ID] = true
		}
		for _, b := range all {
			if seen[b.ID] {
				continue
			}
			cached = append(cached, b)
			seen[b.ID] = true
		}
		return finish(cached)
	}
	c.mu.RUnlock()
	return c.backing.List(query)
}

// CachedList returns query results from the in-memory cache only. The boolean
// reports whether the cache was initialized enough to answer without touching
// the backing store. Dirty entries are returned from the last observed
// snapshot; callers must treat this as a read model that may lag writes or
// reconciliation by one tick.
func (c *CachingStore) CachedList(query ListQuery) ([]Bead, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil, false
	}
	cached := make([]Bead, 0, len(c.beads))
	for _, b := range c.beads {
		if !query.Matches(b) {
			continue
		}
		cached = append(cached, cloneBead(b))
	}
	sortBeadsForQuery(cached, query.Sort)
	if query.Limit > 0 && len(cached) > query.Limit {
		cached = cached[:query.Limit]
	}
	return cached, true
}

func (c *CachingStore) refreshCachedBeads(query ListQuery, startSeq uint64, items []Bead) []Bead {
	refreshedParents := make(map[string]Bead)
	removedParents := make(map[string]struct{})
	for _, id := range c.staleParentCacheIDs(query.ParentID, items) {
		fresh, err := c.backing.Get(id)
		switch {
		case err == nil:
			refreshedParents[id] = cloneBead(fresh)
		case errors.Is(err, ErrNotFound):
			removedParents[id] = struct{}{}
		default:
			c.recordProblem("refresh parent cache during list", fmt.Errorf("%s: %w", id, err))
		}
	}
	if len(items) == 0 && len(refreshedParents) == 0 && len(removedParents) == 0 {
		return items
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != cacheLive && c.state != cachePartial {
		return items
	}
	refreshed := make([]Bead, 0, len(items))
	for _, item := range items {
		switch {
		case c.deletedSeq[item.ID] > startSeq:
			continue
		case c.beadSeq[item.ID] > startSeq:
			current, ok := c.beads[item.ID]
			if ok && query.Matches(current) {
				refreshed = append(refreshed, cloneBead(current))
			}
			continue
		}
		c.beads[item.ID] = cloneBead(item)
		delete(c.dirty, item.ID)
		delete(c.deletedSeq, item.ID)
		delete(c.beadSeq, item.ID)
		if query.Matches(item) {
			refreshed = append(refreshed, cloneBead(item))
		}
	}
	for id, bead := range refreshedParents {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		c.beads[id] = bead
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		delete(c.beadSeq, id)
	}
	for id := range removedParents {
		if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
			continue
		}
		delete(c.beads, id)
		delete(c.deps, id)
		delete(c.dirty, id)
		delete(c.deletedSeq, id)
		delete(c.beadSeq, id)
	}
	c.markFreshLocked(time.Now())
	c.updateStatsLocked()
	return refreshed
}

func (c *CachingStore) staleParentCacheIDs(parentID string, fresh []Bead) []string {
	if parentID == "" {
		return nil
	}

	freshIDs := make(map[string]struct{}, len(fresh))
	for _, item := range fresh {
		freshIDs[item.ID] = struct{}{}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.state != cacheLive && c.state != cachePartial {
		return nil
	}

	var stale []string
	for id, bead := range c.beads {
		if bead.ParentID != parentID {
			continue
		}
		if _, ok := freshIDs[id]; ok {
			continue
		}
		stale = append(stale, id)
	}
	return stale
}

// ListOpen returns all cached beads, optionally filtered by status.
func (c *CachingStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return c.List(query)
}

// Get returns a single bead by ID from the cache or backing store.
func (c *CachingStore) Get(id string) (Bead, error) {
	c.mu.RLock()
	if c.state == cacheLive || c.state == cachePartial {
		if _, ok := c.dirty[id]; ok {
			startSeq := c.mutationSeq
			c.mu.RUnlock()
			fresh, err := c.backing.Get(id)
			if err != nil {
				return Bead{}, err
			}
			c.mu.Lock()
			if c.state != cacheLive && c.state != cachePartial {
				c.mu.Unlock()
				return fresh, nil
			}
			switch {
			case c.deletedSeq[id] > startSeq:
				c.mu.Unlock()
				return Bead{}, ErrNotFound
			case c.beadSeq[id] > startSeq:
				if _, stillDirty := c.dirty[id]; stillDirty {
					c.mu.Unlock()
					return c.backing.Get(id)
				}
				if current, ok := c.beads[id]; ok {
					c.mu.Unlock()
					return cloneBead(current), nil
				}
				c.mu.Unlock()
				return Bead{}, ErrNotFound
			}
			c.beads[id] = cloneBead(fresh)
			delete(c.dirty, id)
			delete(c.deletedSeq, id)
			delete(c.beadSeq, id)
			c.markFreshLocked(time.Now())
			c.updateStatsLocked()
			c.mu.Unlock()
			return fresh, nil
		}
		if b, ok := c.beads[id]; ok {
			c.mu.RUnlock()
			return cloneBead(b), nil
		}
		c.mu.RUnlock()
		return c.backing.Get(id)
	}
	c.mu.RUnlock()
	return c.backing.Get(id)
}

// Ready returns open beads whose blocking deps are all closed.
func (c *CachingStore) Ready() ([]Bead, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		if len(c.dirty) > 0 {
			c.mu.RUnlock()
			return c.backing.Ready()
		}
		statusByID := make(map[string]string, len(c.beads))
		depsByID := make(map[string][]Dep, len(c.deps))
		openBeads := make([]Bead, 0, len(c.beads))
		for _, b := range c.beads {
			statusByID[b.ID] = b.Status
			if b.Status == "open" && !IsReadyExcludedType(b.Type) {
				openBeads = append(openBeads, cloneBead(b))
			}
		}
		for _, b := range openBeads {
			deps := cloneDeps(c.deps[b.ID])
			depsByID[b.ID] = deps
		}
		c.mu.RUnlock()

		var result []Bead
		for _, b := range openBeads {
			blocked := false
			for _, dep := range depsByID[b.ID] {
				switch dep.Type {
				case "blocks", "waits-for", "conditional-blocks":
				default:
					continue
				}
				if status, ok := statusByID[dep.DependsOnID]; ok && status != "closed" {
					blocked = true
					break
				}
			}
			if !blocked {
				result = append(result, cloneBead(b))
			}
		}
		return result, nil
	}
	c.mu.RUnlock()
	return c.backing.Ready()
}

// Children returns beads with the given parent ID.
func (c *CachingStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
	})
}

// ListByLabel returns beads matching the given label. By default, serves from
// cache only (non-closed beads). Pass IncludeClosed to also query the backing
// store for closed beads and merge results.
func (c *CachingStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
	})
}

// ListByAssignee returns beads assigned to the given agent with matching status.
func (c *CachingStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return c.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata filters beads by metadata key-value pairs. By default, serves
// from cache only (non-closed beads). Pass IncludeClosed to also query the
// backing store for closed beads and merge results.
func (c *CachingStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return c.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
	})
}

func matchesMetadata(b Bead, filters map[string]string) bool {
	for k, v := range filters {
		if b.Metadata[k] != v {
			return false
		}
	}
	return true
}

// DepList returns dependencies for a bead in the given direction.
func (c *CachingStore) DepList(id, direction string) ([]Dep, error) {
	c.mu.RLock()
	if c.state == cacheLive {
		if direction == "down" || direction == "" {
			if deps, ok := c.deps[id]; ok {
				c.mu.RUnlock()
				return cloneDeps(deps), nil
			}
			// Dep not cached yet - fetch from backing and cache it.
			c.mu.RUnlock()
			deps, err := c.backing.DepList(id, direction)
			if err != nil {
				return nil, err
			}
			c.mu.Lock()
			c.deps[id] = cloneDeps(deps)
			c.mu.Unlock()
			return deps, nil
		}
		// Reverse lookups are only partially cached; defer to the backing
		// store so callers do not observe incomplete results.
		c.mu.RUnlock()
		return c.backing.DepList(id, direction)
	}
	c.mu.RUnlock()
	return c.backing.DepList(id, direction)
}

// Ping delegates to the backing store.
func (c *CachingStore) Ping() error {
	return c.backing.Ping()
}
