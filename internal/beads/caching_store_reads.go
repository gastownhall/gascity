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
	if query.Live {
		return c.backing.List(query)
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
			c.mu.RUnlock()
			fresh, err := c.backing.Get(id)
			if err != nil {
				return Bead{}, err
			}
			c.mu.Lock()
			c.beads[id] = cloneBead(fresh)
			delete(c.dirty, id)
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
		missingDepIDs := make(map[string]struct{})
		for _, b := range c.beads {
			statusByID[b.ID] = b.Status
			if b.Status == "open" && !IsReadyExcludedType(b.Type) {
				openBeads = append(openBeads, cloneBead(b))
			}
		}
		for _, b := range openBeads {
			deps := cloneDeps(c.deps[b.ID])
			depsByID[b.ID] = deps
			for _, dep := range deps {
				switch dep.Type {
				case "blocks", "waits-for", "conditional-blocks":
				default:
					continue
				}
				if _, ok := statusByID[dep.DependsOnID]; !ok {
					missingDepIDs[dep.DependsOnID] = struct{}{}
				}
			}
		}
		c.mu.RUnlock()

		for depID := range missingDepIDs {
			dep, err := c.backing.Get(depID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return nil, err
			}
			statusByID[depID] = dep.Status
		}

		var result []Bead
		for _, b := range openBeads {
			blocked := false
			for _, dep := range depsByID[b.ID] {
				switch dep.Type {
				case "blocks", "waits-for", "conditional-blocks":
				default:
					continue
				}
				if statusByID[dep.DependsOnID] != "closed" {
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
