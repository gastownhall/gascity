package beads

// Differential gate for the S01 Phase-2 reconcile collapse.
//
// The fleet-critical beads read cache reconciles a full-scan snapshot into six
// in-memory maps. Before Phase 2 this was two branches: Branch A (per-row
// in-place merge, taken when a local write raced the scan) and Branch B
// (whole-map rebuild, taken in the quiescent regime). Phase 2 collapses both
// into a single pipeline routed through the pure reconcileMergeDecision plus a
// fence/deps-GC sweep.
//
// A merge divergence does not crash — it silently serves stale or wrong beads
// to every agent (#2987 class). This gate is the sole quality assurance for
// the collapse. It runs the FROZEN legacy Branch A and Branch B bodies and the
// LIVE collapsed seam on byte-identical inputs and asserts their end-states are
// identical modulo the exactly-enumerated §2 deltas (D1, D1', D2, D3, D3', D4,
// D5), over a provably-covered decision space.
//
// Provenance of the frozen copies: mechanical transliterations of
// runReconciliation's two branches at the pre-collapse commit 84c010a1b
// (internal/beads/caching_store_reconcile.go lines 346-542), extracted
// 2026-07-08. They call the REAL in-package helpers (recentLocalBeadConflictLocked,
// depsForReconcileLocked, carryRecentLocalMutationLocked,
// preserveCachedReadyProjectionLocked, beadChanged, depsChanged, cloneBead,
// cloneDeps, absorbFreshLocked, evictLocked) so helper semantics are shared by
// construction; the differential surface is exactly the branch structure being
// collapsed. Scope: this gate proves branch-structure equivalence GIVEN shared
// helpers; helper semantics are pinned separately by the white-box suite + T4.
//
// DO NOT edit or delete the frozen copies. They are the ongoing guard: every
// CI run re-proves the collapsed loop against Branch A/B semantics.

import (
	"reflect"
	"time"
)

// ---------------------------------------------------------------------------
// Harness state types
// ---------------------------------------------------------------------------

// storeState is the pre-merge cache state the seam reads: the six per-row maps
// plus the two scalars that steer it (depsComplete is written, mutationSeq
// selects the OLD regime). backingIsBd drives depsForReconcileLocked's
// off-BdStore cached-deps fallback and is a shared input to all three
// implementations.
type storeState struct {
	beads        map[string]Bead
	deps         map[string][]Dep
	depsComplete bool
	dirty        map[string]struct{}
	beadSeq      map[string]uint64
	localBeadAt  map[string]time.Time
	deletedSeq   map[string]uint64
	mutationSeq  uint64
	backingIsBd  bool
}

// snapshotInputs is the seam's argument tuple (mergeSnapshotLocked's params).
type snapshotInputs struct {
	freshByID       map[string]Bead
	confirmedClosed map[string]Bead
	depMap          map[string][]Dep
	useFreshDeps    bool
	startSeq        uint64
	now             time.Time
}

// quiescent reports whether the OLD selector would take Branch B.
func (in snapshotInputs) quiescent(st storeState) bool {
	return st.mutationSeq == in.startSeq
}

// mergeEndState is the deterministic post-merge cache state the oracle compares.
// It captures every field the seam writes; the field-coverage census
// (TestMergeOracleFieldCoverage) proves this list stays exhaustive.
type mergeEndState struct {
	beads        map[string]Bead
	deps         map[string][]Dep
	depsComplete bool
	dirty        map[string]struct{}
	beadSeq      map[string]uint64
	localBeadAt  map[string]time.Time
	deletedSeq   map[string]uint64
	state        cacheState
	lastFreshAt  time.Time
	mutationSeq  uint64
	primeErr     string
	syncFailures int
	// stats fields the seam writes.
	statsAdds            int64
	statsRemoves         int64
	statsUpdates         int64
	statsLastReconcileAt time.Time
	statsLastFreshAt     time.Time
}

// ---------------------------------------------------------------------------
// Deep clone (so the three runs see byte-identical, independent inputs)
// ---------------------------------------------------------------------------

func cloneBeadMap(m map[string]Bead) map[string]Bead {
	if m == nil {
		return nil
	}
	out := make(map[string]Bead, len(m))
	for k, v := range m {
		out[k] = cloneBead(v)
	}
	return out
}

func cloneDepMap(m map[string][]Dep) map[string][]Dep {
	if m == nil {
		return nil
	}
	out := make(map[string][]Dep, len(m))
	for k, v := range m {
		// Match the production cloneDeps helper: an empty entry (nil or []Dep{})
		// clones to nil, a non-empty one is copied. This mirrors what the live
		// seam stores, so the differential oracle reasons about the same
		// normalized deps. Key presence is preserved; the empty-vs-nil value
		// distinction is intentionally collapsed, exactly as the seam collapses it.
		out[k] = cloneDeps(v)
	}
	return out
}

func cloneDirty(m map[string]struct{}) map[string]struct{} {
	if m == nil {
		return nil
	}
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func cloneU64Map(m map[string]uint64) map[string]uint64 {
	if m == nil {
		return nil
	}
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneTimeMap(m map[string]time.Time) map[string]time.Time {
	if m == nil {
		return nil
	}
	out := make(map[string]time.Time, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneStoreState(st storeState) storeState {
	return storeState{
		beads:        cloneBeadMap(st.beads),
		deps:         cloneDepMap(st.deps),
		depsComplete: st.depsComplete,
		dirty:        cloneDirty(st.dirty),
		beadSeq:      cloneU64Map(st.beadSeq),
		localBeadAt:  cloneTimeMap(st.localBeadAt),
		deletedSeq:   cloneU64Map(st.deletedSeq),
		mutationSeq:  st.mutationSeq,
		backingIsBd:  st.backingIsBd,
	}
}

func cloneSnapshotInputs(in snapshotInputs) snapshotInputs {
	return snapshotInputs{
		freshByID:       cloneBeadMap(in.freshByID),
		confirmedClosed: cloneBeadMap(in.confirmedClosed),
		depMap:          cloneDepMap(in.depMap),
		useFreshDeps:    in.useFreshDeps,
		startSeq:        in.startSeq,
		now:             in.now,
	}
}

// ---------------------------------------------------------------------------
// Harness store construction + end-state capture
// ---------------------------------------------------------------------------

// countingBacking wraps a Store and counts Get/List so the merge-purity
// assertion can prove the seam performs zero backing I/O. Every other Store
// method delegates through the embedded interface.
type countingBacking struct {
	Store
	inner Store
	gets  int
	lists int
}

func (b *countingBacking) Get(id string) (Bead, error) {
	b.gets++
	return b.inner.Get(id)
}

func (b *countingBacking) List(q ListQuery) ([]Bead, error) {
	b.lists++
	return b.inner.List(q)
}

// newMergeHarnessStore builds a CachingStore seeded directly from st. The
// backing is a call-counting fake for the off-BdStore case; for backingIsBd
// it is a nil *BdStore whose only in-seam use is depsForReconcileLocked's
// type assertion (no call), so a stray call would panic — a louder failure
// than a count mismatch. The store starts cacheLive (promoteLiveLocked
// overwrites it regardless).
func newMergeHarnessStore(st storeState) (*CachingStore, *countingBacking) {
	var counter *countingBacking
	var backing Store
	if st.backingIsBd {
		backing = (*BdStore)(nil)
	} else {
		backingTruth := NewMemStore()
		counter = &countingBacking{Store: backingTruth, inner: backingTruth}
		backing = counter
	}
	c := &CachingStore{
		backing:      backing,
		beads:        cloneBeadMap(st.beads),
		deps:         cloneDepMap(st.deps),
		depsComplete: st.depsComplete,
		dirty:        cloneDirty(st.dirty),
		beadSeq:      cloneU64Map(st.beadSeq),
		localBeadAt:  cloneTimeMap(st.localBeadAt),
		deletedSeq:   cloneU64Map(st.deletedSeq),
		mutationSeq:  st.mutationSeq,
		state:        cacheLive,
	}
	ensureMaps(c)
	return c, counter
}

// ensureMaps guarantees non-nil maps so seeded-empty states behave like a
// freshly constructed store.
func ensureMaps(c *CachingStore) {
	if c.beads == nil {
		c.beads = make(map[string]Bead)
	}
	if c.deps == nil {
		c.deps = make(map[string][]Dep)
	}
	if c.dirty == nil {
		c.dirty = make(map[string]struct{})
	}
	if c.beadSeq == nil {
		c.beadSeq = make(map[string]uint64)
	}
	if c.localBeadAt == nil {
		c.localBeadAt = make(map[string]time.Time)
	}
	if c.deletedSeq == nil {
		c.deletedSeq = make(map[string]uint64)
	}
}

func captureEndState(c *CachingStore) mergeEndState {
	primeErr := ""
	if c.primePartialErr != nil {
		primeErr = c.primePartialErr.Error()
	}
	return mergeEndState{
		beads:                cloneBeadMap(c.beads),
		deps:                 cloneDepMap(c.deps),
		depsComplete:         c.depsComplete,
		dirty:                cloneDirty(c.dirty),
		beadSeq:              cloneU64Map(c.beadSeq),
		localBeadAt:          cloneTimeMap(c.localBeadAt),
		deletedSeq:           cloneU64Map(c.deletedSeq),
		state:                c.state,
		lastFreshAt:          c.lastFreshAt,
		mutationSeq:          c.mutationSeq,
		primeErr:             primeErr,
		syncFailures:         c.syncFailures,
		statsAdds:            c.stats.Adds,
		statsRemoves:         c.stats.Removes,
		statsUpdates:         c.stats.Updates,
		statsLastReconcileAt: c.stats.LastReconcileAt,
		statsLastFreshAt:     c.stats.LastFreshAt,
	}
}

// ---------------------------------------------------------------------------
// The three implementations under test
// ---------------------------------------------------------------------------

// mergeImpl runs one merge implementation against a store seeded from st with
// snapshot inputs in, and returns the captured end-state, notifications,
// counters, and backing-call counts.
type mergeImplResult struct {
	end           mergeEndState
	notifications []cacheNotification
	backingCalls  int
}

// runNewMerge exercises the LIVE collapsed seam (mergeSnapshotLocked).
func runNewMerge(st storeState, in snapshotInputs) mergeImplResult {
	c, counter := newMergeHarnessStore(st)
	c.mu.Lock()
	res := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
	c.mu.Unlock()
	return mergeImplResult{end: captureEndState(c), notifications: res.notifications, backingCalls: counterCalls(counter)}
}

// runLegacyA exercises the frozen Branch A body.
func runLegacyA(st storeState, in snapshotInputs) mergeImplResult {
	c, counter := newMergeHarnessStore(st)
	c.mu.Lock()
	res := legacyBranchAMerge(c, in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
	c.mu.Unlock()
	return mergeImplResult{end: captureEndState(c), notifications: res.notifications, backingCalls: counterCalls(counter)}
}

// runLegacyB exercises the frozen Branch B body.
func runLegacyB(st storeState, in snapshotInputs) mergeImplResult {
	c, counter := newMergeHarnessStore(st)
	c.mu.Lock()
	res := legacyBranchBMerge(c, in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
	c.mu.Unlock()
	return mergeImplResult{end: captureEndState(c), notifications: res.notifications, backingCalls: counterCalls(counter)}
}

func counterCalls(counter *countingBacking) int {
	if counter == nil {
		return 0
	}
	return counter.gets + counter.lists
}

// ---------------------------------------------------------------------------
// FROZEN legacy Branch A — DO NOT EDIT (see provenance header above)
// ---------------------------------------------------------------------------

// legacyBranchAMerge is the transliteration of the c.mutationSeq != startSeq
// arm of runReconciliation at 84c010a1b, minus the impure tail that stays in
// runReconciliation (backing.List, latency/cadence bookkeeping, the success
// log, notifyChanges). It performs the preserve pass, the per-row in-place
// absorb loop, and the eviction loop, then the tail scalars that the seam
// owns, and returns the notifications + counters. Caller holds c.mu.
func legacyBranchAMerge(
	c *CachingStore,
	freshByID map[string]Bead, confirmedClosed map[string]Bead,
	depMap map[string][]Dep, useFreshDeps bool,
	startSeq uint64, now time.Time,
) mergeSectionResult {
	c.preserveCachedReadyProjectionLocked(freshByID, depMap, useFreshDeps)

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

		c.absorbFreshLocked(id, freshBead, now, absorbOpts{
			depsMode:   depsExplicit,
			deps:       freshDeps,
			seqMode:    seqClearGuarded,
			clearDirty: true,
		})
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
		c.evictLocked(id)
	}

	c.syncFailures = 0
	c.depsComplete = nextDepsComplete
	c.primePartialErr = nil
	c.promoteLiveLocked()
	c.stats.LastReconcileAt = now
	c.stats.Adds += adds
	c.stats.Removes += removes
	c.stats.Updates += updates
	c.markFreshLocked(now)
	return mergeSectionResult{notifications: notifications, adds: adds, removes: removes, updates: updates}
}

// ---------------------------------------------------------------------------
// FROZEN legacy Branch B — DO NOT EDIT (see provenance header above)
// ---------------------------------------------------------------------------

// legacyBranchBMerge is the transliteration of the quiescent (else) arm of
// runReconciliation at 84c010a1b: the whole-map rebuild. Same seam boundary
// as legacyBranchAMerge. Caller holds c.mu.
func legacyBranchBMerge(
	c *CachingStore,
	freshByID map[string]Bead, confirmedClosed map[string]Bead,
	depMap map[string][]Dep, useFreshDeps bool,
	_ uint64, now time.Time,
) mergeSectionResult {
	c.preserveCachedReadyProjectionLocked(freshByID, depMap, useFreshDeps)

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
	c.stats.LastReconcileAt = now
	c.stats.Adds += adds
	c.stats.Removes += removes
	c.stats.Updates += updates
	c.markFreshLocked(now)
	return mergeSectionResult{notifications: notifications, adds: adds, removes: removes, updates: updates}
}

// endStatesEqual is exact structural equality of two captured end-states,
// including nil-vs-empty distinctions for every map (reflect.DeepEqual treats
// nil and empty maps/slices as unequal, which is what depsComplete-degradation
// and entry-presence semantics require). time.Time values are exact copies of
// injected inputs across all runs, so DeepEqual compares them soundly.
func endStatesEqual(a, b mergeEndState) bool {
	return reflect.DeepEqual(a, b)
}
