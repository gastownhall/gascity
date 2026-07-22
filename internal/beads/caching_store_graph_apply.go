package beads

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// GraphApplyHandle returns a cache-coherent graph-apply handle when the
// backing store supports graph apply.
func (c *CachingStore) GraphApplyHandle() (GraphApplyStore, bool) {
	applier, ok := GraphApplyFor(c.backing)
	if !ok {
		return nil, false
	}
	if storageApplier, ok := applier.(StorageGraphApplyStore); ok {
		return cachingStorageGraphApplyStore{
			cachingGraphApplyStore: cachingGraphApplyStore{cache: c, applier: applier},
			storageApplier:         storageApplier,
		}, true
	}
	return cachingGraphApplyStore{cache: c, applier: applier}, true
}

type cachingGraphApplyStore struct {
	cache   *CachingStore
	applier GraphApplyStore
}

func (s cachingGraphApplyStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	unlock := s.cache.lockUnknownMutationScope()
	result, err := s.applier.ApplyGraphPlan(ctx, plan)
	if err != nil {
		unlock()
		return result, err
	}
	drains := s.cache.refreshGraphAppliedBeads(result)
	unlock()
	for _, drain := range drains {
		drain()
	}
	return result, nil
}

func (s cachingGraphApplyStore) SupportsEphemeralGraphApply() bool {
	if supporter, ok := s.applier.(EphemeralGraphApplyStore); ok {
		return supporter.SupportsEphemeralGraphApply()
	}
	return false
}

type cachingStorageGraphApplyStore struct {
	cachingGraphApplyStore
	storageApplier StorageGraphApplyStore
}

func (s cachingStorageGraphApplyStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	unlock := s.cache.lockUnknownMutationScope()
	result, err := s.storageApplier.ApplyGraphPlanWithStorage(ctx, plan, storage)
	if err != nil {
		unlock()
		return result, err
	}
	drains := s.cache.refreshGraphAppliedBeads(result)
	unlock()
	for _, drain := range drains {
		drain()
	}
	return result, nil
}

func (c *CachingStore) refreshGraphAppliedBeads(result *GraphApplyResult) []func() {
	if result == nil || len(result.IDs) == 0 {
		return nil
	}
	ids := uniqueGraphAppliedIDs(result)
	refreshed := make([]txTouchedBead, 0, len(ids))
	var refreshErr error
	for _, id := range ids {
		fresh, deps, ok := c.refreshBeadWithDepsAfterWrite(id, "refresh bead after graph apply")
		item := txTouchedBead{id: id}
		if ok {
			item.bead = fresh
			item.bead.Dependencies = cloneDeps(deps)
			item.found = true
		} else {
			item.err = ErrNotFound
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh bead after graph apply %s: %w", id, ErrNotFound))
		}
		refreshed = append(refreshed, item)
	}

	notifications := make([]cacheNotification, 0, len(refreshed))
	now := time.Now()
	c.mu.Lock()
	c.noteLocalMutationLocked(ids...)
	if refreshErr != nil {
		c.recordProblemLocked("graph apply refresh", refreshErr)
	}
	for _, item := range refreshed {
		if item.found {
			fresh := cloneBead(item.bead)
			c.absorbFreshLocked(item.id, item.bead, now, absorbOpts{
				depsMode:   depsExplicit,
				deps:       item.bead.Dependencies,
				seqMode:    seqKeep,
				clearDirty: true,
			})
			notifications = append(notifications, cacheNotification{
				eventType:      "bead.created",
				bead:           fresh,
				resolvePending: true,
			})
			continue
		}
		c.markDirtyLocked(item.id)
		c.retainPendingPublication(item.id, "bead.created")
	}
	c.markFreshLocked(now)
	c.updateStatsLocked()
	c.mu.Unlock()
	return c.enqueueOrderedChanges(notifications)
}

func uniqueGraphAppliedIDs(result *GraphApplyResult) []string {
	seen := make(map[string]struct{}, len(result.IDs))
	ids := make([]string, 0, len(result.IDs))
	for _, id := range result.IDs {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
