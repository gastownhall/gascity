package beads

import (
	"context"
	"time"
)

func (c *CachingStore) reconcileLoop(ctx context.Context) {
	timer := time.NewTimer(cacheReconcilePollInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		if c.nextReconcileDelay(time.Now()) == 0 && c.reconciling.CompareAndSwap(false, true) {
			c.runReconciliation()
			c.reconciling.Store(false)
		}

		next := c.nextReconcileDelay(time.Now())
		if next <= 0 || next > cacheReconcilePollInterval {
			next = cacheReconcilePollInterval
		}
		timer.Reset(next)
	}
}

func (c *CachingStore) adaptiveIntervalLocked() time.Duration {
	total := len(c.beads)
	switch {
	case total >= 5000:
		return cacheReconcileIntervalLarge
	case total >= 1000:
		return cacheReconcileIntervalMedium
	default:
		return cacheReconcileIntervalSmall
	}
}

func (c *CachingStore) nextReconcileDelay(now time.Time) time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.state == cacheDegraded || c.lastFreshAt.IsZero() {
		return 0
	}

	lastFullScanAt := c.stats.LastReconcileAt
	if lastFullScanAt.IsZero() {
		lastFullScanAt = c.lastFreshAt
	}
	dueAt := lastFullScanAt.Add(c.adaptiveIntervalLocked())
	if !now.Before(dueAt) {
		return 0
	}
	return dueAt.Sub(now)
}

func (c *CachingStore) runReconciliation() {
	start := time.Now()

	c.mu.RLock()
	startSeq := c.mutationSeq
	c.mu.RUnlock()

	fresh, err := c.backing.List(ListQuery{AllowScan: true})
	if err != nil {
		c.mu.Lock()
		c.syncFailures++
		if c.syncFailures >= maxCacheSyncFailures && c.state == cacheLive {
			c.state = cacheDegraded
		}
		c.recordProblemLocked("reconcile cache", err)
		c.updateStatsLocked()
		c.mu.Unlock()
		return
	}

	freshByID := make(map[string]Bead, len(fresh))
	for _, b := range fresh {
		freshByID[b.ID] = cloneBead(b)
	}

	depMap, depErr := c.fetchDepsForIDs(beadIDs(freshByID))
	if depErr != nil {
		c.recordProblem("refresh dep cache during reconcile", depErr)
	}

	c.mu.Lock()
	if c.mutationSeq != startSeq {
		var adds, removes, updates int64
		notifications := make([]cacheNotification, 0, len(freshByID))

		for id, freshBead := range freshByID {
			if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
				continue
			}

			old, exists := c.beads[id]
			switch {
			case !exists:
				adds++
				notifications = append(notifications, cacheNotification{
					eventType: "bead.created",
					bead:      cloneBead(freshBead),
				})
			case beadChanged(old, freshBead):
				updates++
				notifications = append(notifications, cacheNotification{
					eventType: "bead.updated",
					bead:      cloneBead(freshBead),
				})
			case depErr == nil && depsChanged(c.deps[id], depMap[id]):
				updates++
				notifications = append(notifications, cacheNotification{
					eventType: "bead.updated",
					bead:      cloneBead(freshBead),
				})
			}

			c.beads[id] = cloneBead(freshBead)
			if depErr == nil {
				c.deps[id] = cloneDeps(depMap[id])
			}
			delete(c.dirty, id)
			delete(c.deletedSeq, id)
			delete(c.beadSeq, id)
		}

		for id, old := range c.beads {
			if _, exists := freshByID[id]; exists {
				continue
			}
			if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
				continue
			}
			removes++
			if old.Status != "closed" {
				closed := cloneBead(old)
				closed.Status = "closed"
				notifications = append(notifications, cacheNotification{
					eventType: "bead.closed",
					bead:      closed,
				})
			}
			delete(c.beads, id)
			delete(c.deps, id)
			delete(c.dirty, id)
			delete(c.deletedSeq, id)
			delete(c.beadSeq, id)
		}

		c.syncFailures = 0
		if c.state == cacheDegraded {
			c.state = cacheLive
		}
		now := time.Now()
		durMs := float64(time.Since(start).Microseconds()) / 1000.0
		c.stats.LastReconcileAt = now
		c.stats.LastReconcileMs = durMs
		c.stats.Adds += adds
		c.stats.Removes += removes
		c.stats.Updates += updates
		c.markFreshLocked(now)
		c.updateStatsLocked()
		c.mu.Unlock()
		c.notifyChanges(notifications)
		return
	}

	var adds, removes, updates int64
	notifications := make([]cacheNotification, 0, len(freshByID))
	nextDeps := make(map[string][]Dep, len(freshByID))

	for id, freshBead := range freshByID {
		if depErr == nil {
			nextDeps[id] = cloneDeps(depMap[id])
		} else if deps, ok := c.deps[id]; ok {
			nextDeps[id] = cloneDeps(deps)
		}

		old, exists := c.beads[id]
		switch {
		case !exists:
			adds++
			notifications = append(notifications, cacheNotification{
				eventType: "bead.created",
				bead:      cloneBead(freshBead),
			})
		case beadChanged(old, freshBead):
			updates++
			notifications = append(notifications, cacheNotification{
				eventType: "bead.updated",
				bead:      cloneBead(freshBead),
			})
		case depErr == nil && depsChanged(c.deps[id], depMap[id]):
			updates++
			notifications = append(notifications, cacheNotification{
				eventType: "bead.updated",
				bead:      cloneBead(freshBead),
			})
		}
	}

	for id, old := range c.beads {
		if _, exists := freshByID[id]; !exists {
			removes++
			if old.Status == "closed" {
				continue
			}
			closed := cloneBead(old)
			closed.Status = "closed"
			notifications = append(notifications, cacheNotification{
				eventType: "bead.closed",
				bead:      closed,
			})
		}
	}

	c.beads = freshByID
	c.deps = nextDeps
	c.dirty = make(map[string]struct{})
	c.beadSeq = make(map[string]uint64)
	c.deletedSeq = make(map[string]uint64)
	c.syncFailures = 0
	if c.state == cacheDegraded {
		c.state = cacheLive
	}

	now := time.Now()
	durMs := float64(time.Since(start).Microseconds()) / 1000.0
	c.stats.LastReconcileAt = now
	c.stats.LastReconcileMs = durMs
	c.stats.Adds += adds
	c.stats.Removes += removes
	c.stats.Updates += updates
	c.markFreshLocked(now)
	c.updateStatsLocked()
	c.mu.Unlock()
	c.notifyChanges(notifications)
}
