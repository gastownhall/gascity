package beads

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"time"
)

// cacheLatencyWindowSize is the size of the rolling window of bd-list
// durations the reconciler uses for adaptive cadence decisions. Doubles
// as the hysteresis count for demotion.
//
// Rationale (designer §3 hysteresis): the window is asymmetric — a single
// slow scan can promote (P95 over the high-water mark immediately when
// the window fills), but demotion requires N consecutive calm cycles.
// At MEDIUM cadence (60 s) ten cycles is roughly ten minutes of sustained
// low-latency before we trust the easing.
const cacheLatencyWindowSize = 10

// cacheLatencyHighWaterMark is the P95 threshold above which the
// reconciler asks for MEDIUM cadence. Set to cacheReconcileIntervalSmall/4
// (= 7.5 s) per architect §3.2 — a single bd list call taking more than
// a quarter of the small cadence is evidence of sustained backend
// pressure.
const cacheLatencyHighWaterMark = cacheReconcileIntervalSmall / 4

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
	return effectiveCadence(len(c.beads), c.latencyDriverActive)
}

// effectiveCadence composes the bead-count cadence and the latency
// cadence. The result is the slower of the two — either input pushing
// to MEDIUM keeps the cadence at MEDIUM. LARGE is only reachable via
// bead count (>=5000) per architect scope.
func effectiveCadence(beadCount int, latencyDriverActive bool) time.Duration {
	bead := beadCountCadence(beadCount)
	latency := cacheReconcileIntervalSmall
	if latencyDriverActive {
		latency = cacheReconcileIntervalMedium
	}
	if latency > bead {
		return latency
	}
	return bead
}

// beadCountCadence returns the cadence demanded by the bead-count input
// alone. Preserved from the original adaptiveIntervalLocked so the
// classification stays in one place.
func beadCountCadence(total int) time.Duration {
	switch {
	case total >= 5000:
		return cacheReconcileIntervalLarge
	case total >= 1000:
		return cacheReconcileIntervalMedium
	default:
		return cacheReconcileIntervalSmall
	}
}

// recordReconcileLatencyLocked appends a reconcile read sample to the rolling
// latency window, dropping the oldest sample once the window is full. Success
// samples include backing.List plus ready-projection enrichment. Caller must
// hold c.mu (write lock).
func (c *CachingStore) recordReconcileLatencyLocked(d time.Duration) {
	if len(c.latencyWindow) < cacheLatencyWindowSize {
		c.latencyWindow = append(c.latencyWindow, d)
		return
	}
	c.latencyWindow = append(c.latencyWindow[1:], d)
}

// latencyP95Locked returns the nearest-rank P95 of the latency window
// and reports whether the window contains enough samples to be
// meaningful (full to cacheLatencyWindowSize). Caller must hold c.mu.
//
// Nearest-rank P95 index = ceil(0.95 * N) - 1. For N=10 this equals
// len(sorted)-1 (the max), which is why the prior implementation
// happened to be correct at the current window size — but the formula
// generalizes so the function stays P95 if cacheLatencyWindowSize is
// raised later.
func (c *CachingStore) latencyP95Locked() (time.Duration, bool) {
	if len(c.latencyWindow) < cacheLatencyWindowSize {
		return 0, false
	}
	sorted := make([]time.Duration, len(c.latencyWindow))
	copy(sorted, c.latencyWindow)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	return sorted[idx], true
}

// updateCadenceStatsLocked refreshes the diagnostic cadence fields
// without mutating hysteresis state or emitting transition logs. Caller
// must hold c.mu.
func (c *CachingStore) updateCadenceStatsLocked() {
	p95, samplesEnough := c.latencyP95Locked()
	var p95ms float64
	if samplesEnough {
		p95ms = float64(p95.Milliseconds())
	}
	c.stats.CurrentReconcileInterval = effectiveCadence(len(c.beads), c.latencyDriverActive)
	c.stats.LatencyP95Ms = p95ms
	c.stats.CadenceDriver = cadenceDriver(len(c.beads), c.latencyDriverActive)
}

// recomputeCadenceLocked updates the latency-driver hysteresis state
// based on the current P95, recomposes the effective cadence, refreshes
// the diagnostic CacheStats fields, and emits a single transition log
// line on small↔medium changes. Caller must hold c.mu.
//
// Hysteresis is provided by the rolling window itself: a single slow
// scan can promote (P95 jumps the moment the window fills), but
// demotion requires the window to drain — N=cacheLatencyWindowSize
// low-latency cycles before P95 drops below the high-water mark again.
// One spike anywhere in that drain pushes P95 back up and re-arms the
// driver, preventing thrash.
func (c *CachingStore) recomputeCadenceLocked() {
	prev := c.stats.CurrentReconcileInterval
	hadPrev := prev != 0
	prevDriver := c.stats.CadenceDriver
	if prevDriver == "" {
		prevDriver = cadenceDriver(len(c.beads), c.latencyDriverActive)
	}

	p95, samplesEnough := c.latencyP95Locked()
	if samplesEnough {
		if c.latencyDriverActive {
			if p95 <= cacheLatencyHighWaterMark {
				c.latencyDriverActive = false
			}
		} else if p95 > cacheLatencyHighWaterMark {
			c.latencyDriverActive = true
		}
	}

	c.updateCadenceStatsLocked()
	next := c.stats.CurrentReconcileInterval
	driver := cadenceTransitionDriver(prevDriver, c.stats.CadenceDriver)

	if hadPrev && prev != next {
		switch {
		case prev == cacheReconcileIntervalSmall && next == cacheReconcileIntervalMedium:
			log.Printf("beads cache: cadence promoted small→medium driver=%s p95=%.0fms window=%d",
				driver, c.stats.LatencyP95Ms, cacheLatencyWindowSize)
		case prev == cacheReconcileIntervalMedium && next == cacheReconcileIntervalSmall:
			log.Printf("beads cache: cadence demoted medium→small driver=%s p95=%.0fms window=%d",
				driver, c.stats.LatencyP95Ms, cacheLatencyWindowSize)
		}
	}
}

// cadenceDriver classifies which input(s) are driving the current
// cadence. "default" means cadence is at SMALL with no pressure.
func cadenceDriver(beadCount int, latencyDriverActive bool) string {
	beadDrives := beadCountCadence(beadCount) > cacheReconcileIntervalSmall
	switch {
	case beadDrives && latencyDriverActive:
		return "both"
	case beadDrives:
		return "bead-count"
	case latencyDriverActive:
		return "latency"
	default:
		return "default"
	}
}

func cadenceTransitionDriver(prevDriver, nextDriver string) string {
	switch {
	case prevDriver == "both" || nextDriver == "both":
		return "both"
	case nextDriver != "" && nextDriver != "default":
		return nextDriver
	case prevDriver != "" && prevDriver != "default":
		return prevDriver
	default:
		return "default"
	}
}

func (c *CachingStore) nextReconcileDelay(now time.Time) time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.syncFailures >= maxCacheSyncFailures && !c.stats.LastProblemAt.IsZero() {
		dueAt := c.stats.LastProblemAt.Add(cacheReconcileFailureBackoff)
		if !now.Before(dueAt) {
			return 0
		}
		return dueAt.Sub(now)
	}

	if c.state == cacheDegraded {
		return 0
	}

	if c.lastFreshAt.IsZero() {
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

	bdStart := time.Now()
	fresh, err := c.backing.List(ListQuery{AllowScan: true, SkipLabels: true, TierMode: TierBoth})
	if err != nil {
		bdLatency := time.Since(bdStart)
		c.mu.Lock()
		c.syncFailures++
		if (IsPartialResult(err) || c.syncFailures >= maxCacheSyncFailures) && (c.state == cacheLive || c.state == cachePartial) {
			c.state = cacheDegraded
		}
		c.recordProblemLocked("reconcile cache", err)
		c.recordReconcileLatencyLocked(bdLatency)
		c.recomputeCadenceLocked()
		c.updateStatsLocked()
		c.mu.Unlock()
		return
	}
	enriched, enrichErr := c.enrichReadyProjectionForCache(fresh)
	bdLatency := time.Since(bdStart)
	projectionFailed := enrichErr != nil
	if enrichErr != nil {
		c.recordProblem("reconcile ready projection", enrichErr)
	} else {
		fresh = enriched
	}

	freshByID := make(map[string]Bead, len(fresh))
	for _, b := range fresh {
		freshByID[b.ID] = cloneBead(b)
	}

	confirmedClosed := c.recoverMissingFromList(freshByID)

	depMap, depsComplete, depErr := c.fetchDepsForBeads(freshByID)
	if depErr != nil {
		c.recordProblem("refresh dep cache during reconcile", depErr)
	}
	useFreshDeps := depsComplete && depErr == nil

	c.mu.Lock()
	now := time.Now()
	if projectionFailed {
		c.preserveCachedReadyProjectionLocked(freshByID, depMap, useFreshDeps)
	}
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
			if _, keep := c.recentLocalBeadConflictLocked(id, freshBead, now, true); keep {
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
			case beadChanged(old, freshBead, true):
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
				if freshClosed, ok := confirmedClosed[id]; ok {
					closed = cloneBead(freshClosed)
				}
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
		c.promoteLiveLocked()
		durMs := float64(time.Since(start).Microseconds()) / 1000.0
		c.stats.LastReconcileAt = now
		c.stats.LastReconcileMs = durMs
		c.stats.Adds += adds
		c.stats.Removes += removes
		c.stats.Updates += updates
		c.markFreshLocked(now)
		c.recordReconcileLatencyLocked(bdLatency)
		c.recomputeCadenceLocked()
		c.updateStatsLocked()
		logLine, emit := c.reconcileSuccessLogLocked(now, time.Since(start), adds, removes, updates)
		c.mu.Unlock()
		if emit {
			log.Print(logLine)
		}
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
		if current, keep := c.recentLocalBeadConflictLocked(id, freshBead, now, true); keep {
			beadForCache = current
			preservedRecentLocal = true
			c.carryRecentLocalMutationLocked(id, nextDirty, nextBeadSeq, nextLocalBeadAt)
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
		case !preservedRecentLocal && beadChanged(old, freshBead, true):
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
			if freshClosed, ok := confirmedClosed[id]; ok {
				closed = cloneBead(freshClosed)
			}
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
	c.promoteLiveLocked()

	durMs := float64(time.Since(start).Microseconds()) / 1000.0
	c.stats.LastReconcileAt = now
	c.stats.LastReconcileMs = durMs
	c.stats.Adds += adds
	c.stats.Removes += removes
	c.stats.Updates += updates
	c.markFreshLocked(now)
	c.recordReconcileLatencyLocked(bdLatency)
	c.recomputeCadenceLocked()
	c.updateStatsLocked()
	logLine, emit := c.reconcileSuccessLogLocked(now, time.Since(start), adds, removes, updates)
	c.mu.Unlock()
	if emit {
		log.Print(logLine)
	}
	c.notifyChanges(notifications)
}

// promoteLiveLocked marks the cache live after a clean full-scan
// reconciliation. A successful reconcile loads the same complete active
// snapshot (identical ListQuery and dep fetch) a successful Prime would,
// so it promotes unconditionally — not just degraded→live but also
// partial/uninitialized→live. This makes the reconciler a convergence
// path for stores whose initial full prime failed or never ran: without
// it such a store serves its PrimeActive-era snapshot indefinitely while
// only event-bus writes update it, and storage-level state created
// before the controller started (e.g. routed pool work awaiting pickup)
// stays invisible until something happens to touch the bead. Caller must
// hold c.mu (write lock).
func (c *CachingStore) promoteLiveLocked() {
	c.state = cacheLive
}

// reconcileSuccessLogLocked composes the per-reconcile success log line
// and returns (line, true) when emission is permitted by the
// cacheReconcileSuccessLogWindow rate limiter, or ("", false) otherwise.
// Updates lastReconcileLogAt on emit. Caller must hold c.mu.
//
// Gap context: runReconciliation previously emitted no log line on
// successful cache refresh. Cadence transitions and errors were logged,
// but a reconciler ticking quietly with stale data produced no operator-
// visible signal. On a T7920 incident 2026-05-26 a stale cache went
// undetected for 2h 31m. This line gives the operator a heartbeat plus
// diff counts and bd-list duration without flooding the log.
func (c *CachingStore) reconcileSuccessLogLocked(now time.Time, elapsed time.Duration, adds, removes, updates int64) (string, bool) {
	if !c.lastReconcileLogAt.IsZero() && now.Sub(c.lastReconcileLogAt) < cacheReconcileSuccessLogWindow {
		return "", false
	}
	c.lastReconcileLogAt = now
	rig := c.idPrefix
	if rig == "" {
		rig = "(no-prefix)"
	}
	cadence := c.stats.CadenceDriver
	if cadence == "" {
		cadence = "default"
	}
	return fmt.Sprintf(
		"beads cache: reconciled rig=%s beads=%d adds=%d updates=%d removes=%d took=%s cadence=%s",
		rig, len(c.beads), adds, updates, removes, elapsed.Round(time.Millisecond), cadence,
	), true
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
// bead.closed as before. When Get confirms a closed bead, the returned map
// carries that fresh row so the diff path can emit an authoritative close
// payload instead of a stale cached status flip. On any other error the cached
// entry is merged back conservatively, deferring the close to a later scan
// when the backing store's state is unambiguous. Callers must own freshByID
// and not access it concurrently while recovery is running.
func (c *CachingStore) recoverMissingFromList(freshByID map[string]Bead) map[string]Bead {
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
		return nil
	}
	var confirmedClosed map[string]Bead
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
				if confirmedClosed == nil {
					confirmedClosed = make(map[string]Bead)
				}
				confirmedClosed[id] = cloneBead(bead)
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
	return confirmedClosed
}

func (c *CachingStore) preserveCachedReadyProjectionLocked(items map[string]Bead, depMap map[string][]Dep, useFreshDeps bool) {
	for id, item := range items {
		if item.IsBlocked != nil {
			continue
		}
		cached, ok := c.beads[id]
		if !ok || cached.IsBlocked == nil {
			continue
		}
		freshDeps := c.depsForReconcileLocked(id, item, depMap, useFreshDeps)
		if depsChanged(c.deps[id], freshDeps) {
			continue
		}
		if c.readyBlockingDependencyTargetStatusChangedLocked(freshDeps, items) {
			continue
		}
		item.IsBlocked = cloneBoolPtr(cached.IsBlocked)
		items[id] = item
	}
}

func (c *CachingStore) readyBlockingDependencyTargetStatusChangedLocked(deps []Dep, items map[string]Bead) bool {
	for _, dep := range deps {
		if !isReadyBlockingDependencyType(dep.Type) {
			continue
		}
		cachedTarget, cachedOK := c.beads[dep.DependsOnID]
		freshTarget, freshOK := items[dep.DependsOnID]
		if !freshOK {
			continue
		}
		if !cachedOK {
			return true
		}
		if cachedTarget.Status != freshTarget.Status {
			return true
		}
	}
	return false
}
