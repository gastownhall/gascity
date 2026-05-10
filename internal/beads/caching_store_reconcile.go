package beads

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (c *CachingStore) reconcileLoop(ctx context.Context, stagger time.Duration) {
	if stagger > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(stagger):
		}
	}

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
		if (IsPartialResult(err) || c.syncFailures >= maxCacheSyncFailures) && (c.state == cacheLive || c.state == cachePartial) {
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

	c.recoverMissingFromList(freshByID)

	depMap, depsComplete, depErr := c.fetchDepsForIDs(beadIDs(freshByID))
	if depErr != nil {
		c.recordProblem("refresh dep cache during reconcile", depErr)
	}
	useFreshDeps := depsComplete && depErr == nil

	c.mu.Lock()
	now := time.Now()
	if c.mutationSeq != startSeq {
		var adds, removes, updates int64
		notifications := make([]cacheNotification, 0, len(freshByID))
		nextDepsComplete := useFreshDeps

		for id, freshBead := range freshByID {
			if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
				if _, exists := c.beads[id]; exists {
					if _, ok := c.deps[id]; !ok {
						nextDepsComplete = false
					}
				}
				continue
			}
			if _, keep := c.recentLocalBeadConflictLocked(id, freshBead, now); keep {
				if _, ok := c.deps[id]; !ok {
					nextDepsComplete = false
				}
				continue
			}
			freshDeps := c.depsForReconcileLocked(id, freshBead, depMap, useFreshDeps)

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
			case depsChanged(c.deps[id], freshDeps):
				updates++
				notifications = append(notifications, cacheNotification{
					eventType: "bead.updated",
					bead:      cloneBead(freshBead),
				})
			}

			c.beads[id] = cloneBead(freshBead)
			c.deps[id] = cloneDeps(freshDeps)
			delete(c.dirty, id)
			delete(c.deletedSeq, id)
			if !recentLocalMutation(c.localBeadAt[id], now) {
				delete(c.beadSeq, id)
				delete(c.localBeadAt, id)
			}
		}

		for id, old := range c.beads {
			if _, exists := freshByID[id]; exists {
				continue
			}
			if c.deletedSeq[id] > startSeq || c.beadSeq[id] > startSeq {
				continue
			}
			if old.Status != "closed" && recentLocalMutation(c.localBeadAt[id], now) {
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
			delete(c.localBeadAt, id)
		}

		c.syncFailures = 0
		c.depsComplete = nextDepsComplete
		c.primePartialErr = nil
		if c.state == cacheDegraded {
			c.state = cacheLive
		}
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
	nextBeads := make(map[string]Bead, len(freshByID))
	nextDeps := make(map[string][]Dep, len(freshByID))
	nextDirty := make(map[string]struct{})
	nextBeadSeq := make(map[string]uint64)
	nextLocalBeadAt := make(map[string]time.Time)

	for id, freshBead := range freshByID {
		beadForCache := freshBead
		preservedRecentLocal := false
		if recentLocalMutation(c.localBeadAt[id], now) {
			c.carryRecentLocalMutationLocked(id, nextDirty, nextBeadSeq, nextLocalBeadAt)
		}
		if current, keep := c.recentLocalBeadConflictLocked(id, freshBead, now); keep {
			beadForCache = current
			preservedRecentLocal = true
		}
		freshDeps := c.depsForReconcileLocked(id, freshBead, depMap, useFreshDeps)
		nextBeads[id] = cloneBead(beadForCache)
		nextDeps[id] = cloneDeps(freshDeps)

		old, exists := c.beads[id]
		switch {
		case !exists:
			adds++
			notifications = append(notifications, cacheNotification{
				eventType: "bead.created",
				bead:      cloneBead(beadForCache),
			})
		case !preservedRecentLocal && beadChanged(old, freshBead):
			updates++
			notifications = append(notifications, cacheNotification{
				eventType: "bead.updated",
				bead:      cloneBead(freshBead),
			})
		case !preservedRecentLocal && depsChanged(c.deps[id], freshDeps):
			updates++
			notifications = append(notifications, cacheNotification{
				eventType: "bead.updated",
				bead:      cloneBead(freshBead),
			})
		}
	}

	for id, old := range c.beads {
		if _, exists := freshByID[id]; !exists {
			if old.Status != "closed" && recentLocalMutation(c.localBeadAt[id], now) {
				nextBeads[id] = cloneBead(old)
				if deps, ok := c.deps[id]; ok {
					nextDeps[id] = cloneDeps(deps)
				}
				c.carryRecentLocalMutationLocked(id, nextDirty, nextBeadSeq, nextLocalBeadAt)
				continue
			}
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

	c.beads = nextBeads
	c.deps = nextDeps
	c.depsComplete = useFreshDeps
	c.dirty = nextDirty
	c.beadSeq = nextBeadSeq
	c.localBeadAt = nextLocalBeadAt
	c.deletedSeq = make(map[string]uint64)
	c.syncFailures = 0
	c.primePartialErr = nil
	if c.state == cacheDegraded {
		c.state = cacheLive
	}

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

func (c *CachingStore) depsForReconcileLocked(id string, freshBead Bead, depMap map[string][]Dep, useFreshDeps bool) []Dep {
	if useFreshDeps {
		return cloneDeps(depMap[id])
	}
	freshDeps := depsFromBeadFields(freshBead)
	if _, ok := c.backing.(*BdStore); ok {
		return freshDeps
	}
	if len(freshDeps) == 0 {
		if cachedDeps, ok := c.deps[id]; ok && len(cachedDeps) > 0 {
			return cloneDeps(cachedDeps)
		}
	}
	return freshDeps
}

// recoverMissingFromList re-fetches any cached active bead that didn't appear
// in freshByID and merges verified-alive ones back. This guards against
// cleanly incomplete List results: a List that drops an active bead must not
// synthesize a spurious bead.closed event for it.
//
// On ErrNotFound the bead is left absent so the diff path can emit
// bead.closed as before. On any other error the cached entry is merged
// back conservatively, deferring the close to a later scan when the
// backing store's state is unambiguous. Callers must own freshByID and not
// access it concurrently while recovery is running.
func (c *CachingStore) recoverMissingFromList(freshByID map[string]Bead) {
	c.mu.RLock()
	candidates := make(map[string]Bead)
	for id, b := range c.beads {
		if _, ok := freshByID[id]; ok {
			continue
		}
		if b.Status == "closed" {
			continue
		}
		candidates[id] = cloneBead(b)
	}
	c.mu.RUnlock()
	if len(candidates) == 0 {
		return
	}
	var recoveredAlive int64
	var deferredClose int64
	for id, cached := range candidates {
		bead, err := c.backing.Get(id)
		switch {
		case err == nil:
			if bead.ID != id {
				c.recordProblem(
					"verify missing bead before close",
					fmt.Errorf("%s: backing returned bead %q", id, bead.ID),
				)
				freshByID[id] = cached
				deferredClose++
				continue
			}
			if bead.Status == "closed" {
				continue
			}
			freshByID[id] = cloneBead(bead)
			recoveredAlive++
		case errors.Is(err, ErrNotFound):
			// Confirmed gone; let the diff path emit bead.closed.
		default:
			c.recordProblem(
				"verify missing bead before close",
				fmt.Errorf("%s: %w", id, err),
			)
			freshByID[id] = cached
			deferredClose++
		}
	}
	if recoveredAlive != 0 || deferredClose != 0 {
		c.mu.Lock()
		c.stats.ReconcileRecoveries += recoveredAlive
		c.stats.ReconcileCloseDeferrals += deferredClose
		c.mu.Unlock()
	}
}
