package beads

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/telemetry"
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

// cacheReconcileScanWarnThreshold is the active-bead count at which a
// reconcile full scan emits beads.cache.scan_large telemetry. Sits between
// the bead-count cadence thresholds (MEDIUM at 1000, LARGE at 5000): healthy
// large rigs above the MEDIUM floor stay quiet, while a store drifting toward
// LARGE warns before every cycle pays multi-second, multi-MB bd round-trips
// (ga-698fl2: a dev store silently reached 3,272 active beads / ~11MB of
// JSON / ~2s bd latency per cycle).
const cacheReconcileScanWarnThreshold = 2500

// recordCacheScanLarge emits the over-threshold scan-size telemetry; a var so
// internal tests can intercept emission. Swaps are unsynchronized: tests that
// replace it must stay sequential (no t.Parallel) and must not leave a
// reconcile loop running across the swap.
var recordCacheScanLarge = telemetry.RecordCacheScanLarge

// cacheFullScanQuery is the single query shape Prime and the reconciler use
// to load the cache's authoritative snapshot. The reconcile diff treats the
// result as the COMPLETE active universe: any cached bead absent from it is
// re-verified per ID (recoverMissingFromList) and then evicted with a
// synthetic bead.closed event. Two bounds follow from that authority:
//
//   - Limit must stay unset (0). A bounded list would route every active
//     bead beyond the limit through the per-bead Get recovery path on every
//     cycle — O(active−limit) bd round-trips — and synthesize false
//     bead.closed evictions whenever those Gets degrade.
//   - IncludeClosed is pinned false. The scan cost is O(active beads) by
//     design; closed history grows without bound and would multiply the
//     per-cycle bd payload without changing the diff result.
func cacheFullScanQuery() ListQuery {
	return ListQuery{AllowScan: true, SkipLabels: true, IncludeClosed: false, TierMode: TierBoth}
}

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
	fresh, err := c.backing.List(cacheFullScanQuery())
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
	if len(fresh) >= cacheReconcileScanWarnThreshold {
		recordCacheScanLarge(context.Background(), c.idPrefix, len(fresh),
			cacheReconcileScanWarnThreshold, time.Since(bdStart))
	}
	enriched, enrichErr := c.enrichReadyProjectionForCache(fresh)
	bdLatency := time.Since(bdStart)
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
	res := c.mergeSnapshotLocked(freshByID, confirmedClosed, depMap, useFreshDeps, startSeq, now)
	durMs := float64(time.Since(start).Microseconds()) / 1000.0
	c.stats.LastReconcileMs = durMs
	c.recordReconcileLatencyLocked(bdLatency)
	c.recomputeCadenceLocked()
	c.updateStatsLocked()
	logLine, emit := c.reconcileSuccessLogLocked(now, time.Since(start), res.adds, res.removes, res.updates)
	c.mu.Unlock()
	if emit {
		log.Print(logLine)
	}
	c.notifyChanges(res.notifications)
}

// mergeAction is what the reconcile merge does with one id.
type mergeAction int

const (
	// mergeAbsorb installs the fresh row via
	// absorbFreshLocked{depsExplicit freshDeps, seqClearGuarded, clearDirty:true}.
	mergeAbsorb mergeAction = iota
	// mergeEvict removes the cached row via evictLocked.
	mergeEvict
	// mergeSkipFenced leaves everything for id untouched: a tombstone or
	// beadSeq fence > startSeq proves local state is newer than the snapshot.
	mergeSkipFenced
	// mergeSkipRecentLocal leaves everything for id untouched: the recency
	// window (5 s) protects an in-flight local write bd may not reflect yet.
	mergeSkipRecentLocal
	// mergeGCFences drops every orphan fence/deps entry for id (deletedSeq,
	// dirty, beadSeq, localBeadAt, deps). Only reachable when id has no row on
	// either side.
	mergeGCFences
)

// mergeDecision is the pure per-id verdict of reconcileMergeDecision. Payload
// assembly (confirmedClosed override, cloneBead) and counter bookkeeping stay
// at the seam call site.
type mergeDecision struct {
	action mergeAction
	// notification is the event type to synthesize: "", "bead.created",
	// "bead.updated", or "bead.closed".
	notification string
	// degradeDepsComplete reports that this skip leaves the cached deps map an
	// unfaithful projection of the fresh full scan, so the pass must fold
	// nextDepsComplete = false and dep readers fall back to the backing. Two
	// shapes trip it: a coverage hole (cached row with no deps entry), and a
	// recency-keep that retains cached deps which diverge from the fresh
	// snapshot's deps (the row's body is kept as local truth, but its deps can
	// no longer be claimed complete). The first shape matches the two Branch-A
	// skip-arm degradations; the second closes the D4 contract gap where a
	// recency-keep could serve stale cached deps under depsComplete=true.
	degradeDepsComplete bool
}

// mergeRowInput is the complete per-id state the decision depends on.
// Everything is a value; zero values are the documented "absent" sentinels
// (mutationSeq starts at 1 — noteMutationLocked pre-increments — so seq 0
// means "no entry"; time.Time zero means "no recency stamp").
type mergeRowInput struct {
	freshExists   bool // id present in freshByID (post-recoverMissingFromList)
	fresh         Bead
	freshDeps     []Dep // depsForReconcileLocked output, computed by caller
	cachedExists  bool  // id present in c.beads
	cached        Bead
	cachedDeps    []Dep // c.deps[id] value (nil when absent)
	hasCachedDeps bool  // c.deps[id] presence — distinct from nil/empty value
	deletedAtSeq  uint64
	beadAtSeq     uint64
	startSeq      uint64
	localAt       time.Time
	now           time.Time // the single pass-level clock read
	skipLabels    bool
}

// reconcileMergeDecision decides the fate of one id's state transition in the
// collapsed reconcile: the absorb loop, the eviction loop, and the fence/deps
// GC sweep all route through it. It is pure — no receiver, no locks, no map
// mutation, no clock reads, no I/O — so it is exhaustively enumerable and
// trivially comparable in the differential gate. The fence ordering in each
// case is tombstone/seq fence beats recency beats mutate.
func reconcileMergeDecision(in mergeRowInput) mergeDecision {
	switch {
	case in.freshExists: // absorb-loop cell
		if in.deletedAtSeq > in.startSeq || in.beadAtSeq > in.startSeq {
			return mergeDecision{
				action:              mergeSkipFenced,
				degradeDepsComplete: in.cachedExists && !in.hasCachedDeps,
			}
		}
		if in.cachedExists &&
			recentLocalMutation(in.localAt, in.now) &&
			beadChanged(in.cached, in.fresh, in.skipLabels) {
			return mergeDecision{
				action:              mergeSkipRecentLocal,
				degradeDepsComplete: !in.hasCachedDeps || depsChanged(in.cachedDeps, in.freshDeps),
			}
		}
		n := ""
		switch {
		case !in.cachedExists:
			n = "bead.created"
		case beadChanged(in.cached, in.fresh, in.skipLabels):
			n = "bead.updated"
		case depsChanged(in.cachedDeps, in.freshDeps):
			n = "bead.updated"
		}
		return mergeDecision{action: mergeAbsorb, notification: n}

	case in.cachedExists: // eviction-loop cell (id absent from snapshot)
		if in.deletedAtSeq > in.startSeq || in.beadAtSeq > in.startSeq {
			return mergeDecision{action: mergeSkipFenced}
		}
		if in.cached.Status != "closed" && recentLocalMutation(in.localAt, in.now) {
			return mergeDecision{action: mergeSkipRecentLocal}
		}
		n := ""
		if in.cached.Status != "closed" {
			n = "bead.closed"
		}
		return mergeDecision{action: mergeEvict, notification: n}

	default: // fence-GC cell (no row on either side; orphan fence/deps only)
		if in.deletedAtSeq > in.startSeq || in.beadAtSeq > in.startSeq {
			return mergeDecision{action: mergeSkipFenced}
		}
		if recentLocalMutation(in.localAt, in.now) {
			return mergeDecision{action: mergeSkipRecentLocal}
		}
		return mergeDecision{action: mergeGCFences}
	}
}

// mergeSectionResult carries the deterministic outputs of mergeSnapshotLocked
// back to runReconciliation: the notifications to emit after unlock and the
// per-pass add/remove/update counts.
type mergeSectionResult struct {
	notifications []cacheNotification
	adds          int64
	removes       int64
	updates       int64
}

// mergeSnapshotLocked applies a full-scan snapshot to the cache under c.mu.
// It is the deterministic seam of runReconciliation: pure in-memory, no I/O,
// no clock reads (now injected), no notifications emitted (returned for the
// caller to emit after unlock). Every per-id fate is decided by
// reconcileMergeDecision; the three index sets it iterates (freshByID, the
// cached rows absent from freshByID, and the orphan fence/deps ids) are
// pairwise disjoint, so the passes cannot perturb each other. Caller must hold
// c.mu (write lock).
func (c *CachingStore) mergeSnapshotLocked(
	freshByID map[string]Bead, confirmedClosed map[string]Bead,
	depMap map[string][]Dep, useFreshDeps bool,
	startSeq uint64, now time.Time,
) mergeSectionResult {
	// Preserve a cached is_blocked for any row the projection did not return
	// this cycle. Two cases land here: a full projection failure (enrichErr
	// left every row unenriched) and the narrower race where a row is still
	// open in the list snapshot but closes before the bounded active-row
	// projection query, so the SQL no longer returns it. Without preservation
	// the row's is_blocked flips false->nil and beadChanged emits a spurious
	// bead.updated. The guards inside drop the preservation when the row's deps
	// or a blocking target's status actually changed, so a real transition is
	// never masked. Runs first, on pre-merge state, because it reads other
	// rows' cached status.
	c.preserveCachedReadyProjectionLocked(freshByID, depMap, useFreshDeps)

	res := mergeSectionResult{notifications: make([]cacheNotification, 0, len(freshByID))}
	nextDepsComplete := useFreshDeps

	// 1. Absorb loop — over freshByID. Classification reads pre-absorb state.
	for id, freshBead := range freshByID {
		freshDeps := c.depsForReconcileLocked(id, freshBead, depMap, useFreshDeps)
		cached, cachedExists := c.beads[id]
		cachedDeps, hasCachedDeps := c.deps[id]
		d := reconcileMergeDecision(mergeRowInput{
			freshExists:   true,
			fresh:         freshBead,
			freshDeps:     freshDeps,
			cachedExists:  cachedExists,
			cached:        cached,
			cachedDeps:    cachedDeps,
			hasCachedDeps: hasCachedDeps,
			deletedAtSeq:  c.deletedSeq[id],
			beadAtSeq:     c.beadSeq[id],
			startSeq:      startSeq,
			localAt:       c.localBeadAt[id],
			now:           now,
			skipLabels:    true,
		})
		if d.degradeDepsComplete {
			nextDepsComplete = false
		}
		if d.action != mergeAbsorb {
			continue
		}
		switch d.notification {
		case "bead.created":
			res.adds++
			res.notifications = append(res.notifications, cacheNotification{
				eventType: "bead.created",
				bead:      cloneBead(freshBead),
			})
		case "bead.updated":
			res.updates++
			res.notifications = append(res.notifications, cacheNotification{
				eventType: "bead.updated",
				bead:      cloneBead(freshBead),
			})
		}
		c.absorbFreshLocked(id, freshBead, now, absorbOpts{
			depsMode:   depsExplicit,
			deps:       freshDeps,
			seqMode:    seqClearGuarded,
			clearDirty: true,
		})
	}

	// 2. Eviction loop — over c.beads \ freshByID. Deleting the current key
	//    inside range c.beads is safe per the Go spec.
	for id, cached := range c.beads {
		if _, exists := freshByID[id]; exists {
			continue
		}
		d := reconcileMergeDecision(mergeRowInput{
			freshExists:  false,
			cachedExists: true,
			cached:       cached,
			deletedAtSeq: c.deletedSeq[id],
			beadAtSeq:    c.beadSeq[id],
			startSeq:     startSeq,
			localAt:      c.localBeadAt[id],
			now:          now,
			skipLabels:   true,
		})
		if d.action != mergeEvict {
			continue
		}
		res.removes++
		if d.notification == "bead.closed" {
			closed := cloneBead(cached)
			closed.Status = "closed"
			if freshClosed, ok := confirmedClosed[id]; ok {
				closed = cloneBead(freshClosed)
			}
			res.notifications = append(res.notifications, cacheNotification{
				eventType: "bead.closed",
				bead:      closed,
			})
		}
		c.evictLocked(id)
	}

	// 3. Fence/deps-GC sweep — over orphan ids (a fence or deps entry with no
	//    row on either side). Replaces Branch B's implicit wholesale reset:
	//    stale orphans are collected, recent ones kept one more cycle. The id
	//    set is snapshotted before deleting to avoid iterate-while-delete.
	for _, id := range c.orphanFenceIDsLocked(freshByID) {
		d := reconcileMergeDecision(mergeRowInput{
			freshExists:  false,
			cachedExists: false,
			deletedAtSeq: c.deletedSeq[id],
			beadAtSeq:    c.beadSeq[id],
			startSeq:     startSeq,
			localAt:      c.localBeadAt[id],
			now:          now,
			skipLabels:   true,
		})
		if d.action != mergeGCFences {
			continue
		}
		delete(c.deletedSeq, id)
		delete(c.dirty, id)
		delete(c.beadSeq, id)
		delete(c.localBeadAt, id)
		delete(c.deps, id)
	}

	// 4. Shared tail (was duplicated per branch).
	c.syncFailures = 0
	c.depsComplete = nextDepsComplete
	c.primePartialErr = nil
	c.promoteLiveLocked()
	c.stats.LastReconcileAt = now
	c.stats.Adds += res.adds
	c.stats.Removes += res.removes
	c.stats.Updates += res.updates
	c.markFreshLocked(now)
	return res
}

// orphanFenceIDsLocked returns the ids carrying a fence or deps entry but no
// cached row and no fresh row this cycle — the fence/deps-GC sweep's work set.
// Caller must hold c.mu.
func (c *CachingStore) orphanFenceIDsLocked(freshByID map[string]Bead) []string {
	seen := make(map[string]struct{})
	add := func(id string) {
		if _, ok := c.beads[id]; ok {
			return
		}
		if _, ok := freshByID[id]; ok {
			return
		}
		seen[id] = struct{}{}
	}
	for id := range c.deletedSeq {
		add(id)
	}
	for id := range c.dirty {
		add(id)
	}
	for id := range c.beadSeq {
		add(id)
	}
	for id := range c.localBeadAt {
		add(id)
	}
	for id := range c.deps {
		add(id)
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids
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
