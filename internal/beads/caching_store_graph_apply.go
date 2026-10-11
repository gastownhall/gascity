package beads

import (
	"context"
	"errors"
	"fmt"
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
	startSeq := s.cache.currentMutationSeq()
	result, err := s.applier.ApplyGraphPlan(ctx, plan)
	if err != nil {
		return result, err
	}
	s.cache.refreshGraphAppliedBeads(result, startSeq)
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
	startSeq := s.cache.currentMutationSeq()
	result, err := s.storageApplier.ApplyGraphPlanWithStorage(ctx, plan, storage)
	if err != nil {
		return result, err
	}
	s.cache.refreshGraphAppliedBeads(result, startSeq)
	return result, nil
}

func (c *CachingStore) refreshGraphAppliedBeads(result *GraphApplyResult, startSeq uint64) {
	if result == nil || len(result.IDs) == 0 {
		return
	}
	ids := uniqueGraphAppliedIDs(result)
	refreshed := make([]txTouchedBead, 0, len(ids))
	// edgesUnread names the rows read back without their edges: they notify
	// but stay marked rather than install.
	edgesUnread := make(map[string]struct{})
	var refreshErr error
	for _, id := range ids {
		fresh, deps, ok := c.refreshBeadWithDepsAfterWrite(id, "refresh bead after graph apply")
		item := txTouchedBead{id: id}
		switch {
		case ok:
			item.bead = fresh
			item.bead.Dependencies = cloneDeps(deps)
			item.found = true
		case fresh.ID != "":
			item.bead = fresh
			item.found = true
			edgesUnread[id] = struct{}{}
		default:
			item.err = ErrNotFound
			refreshErr = errors.Join(refreshErr, fmt.Errorf("refresh bead after graph apply %s: %w", id, ErrNotFound))
		}
		refreshed = append(refreshed, item)
	}

	notifications := make([]cacheNotification, 0, len(refreshed))
	now := c.clockNow()
	c.mu.Lock()
	raced := c.racedWritesLocked(ids, startSeq)
	c.noteLocalMutationLocked(ids...)
	if refreshErr != nil {
		c.recordProblemLocked("graph apply refresh", refreshErr)
	}
	for _, item := range refreshed {
		_, skip := raced[item.id]
		if item.found {
			fresh := cloneBead(item.bead)
			_, unread := edgesUnread[item.id]
			switch {
			case skip:
			case unread:
				c.markDirtyLocked(item.id)
			default:
				c.absorbFreshLocked(item.id, item.bead, now, absorbOpts{
					depsMode:   depsExplicit,
					deps:       item.bead.Dependencies,
					seqMode:    seqKeep,
					clearDirty: true,
				})
			}
			notifications = append(notifications, cacheNotification{
				eventType: "bead.created",
				bead:      fresh,
			})
			continue
		}
		if !skip {
			c.markDirtyLocked(item.id)
		}
	}
	c.markFreshLocked(now)
	c.updateStatsLocked()
	c.mu.Unlock()
	c.notifyChanges(ChangeLocal, notifications)
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
