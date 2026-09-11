package beads

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// coverageKey is the discretized per-row decision cell. The guard dimensions
// (regime, presence, tomb, seqFence, recency, changed) are the axes the
// user-visible guards actually branch on; the differential gate requires the
// full V-filtered cross of those. The remaining "soft" dimensions prove
// insensitivity / exercise the council-hardened sub-axes and are required only
// marginally (each value observed at least once).
type coverageKey struct {
	regime     string
	presence   string
	tomb       string
	seqFence   string
	recency    string
	changed    string
	statusPair string

	// Soft / hardened axes (marginal coverage).
	dirty           bool
	depMapCell      string // fresh deps via depMap (useFreshDeps): na|nil|empty|nonempty
	fieldDeps       string // council axis-9 split: fresh bead dep fields: na|none|needs|deps|both
	cachedDeps      string // cached c.deps[id]: absent|nil|empty|nonempty
	useFreshDeps    bool
	backingIsBd     bool
	confirmedClosed bool
	preserveOutcome string
}

// guardCell is the projection over the guard-critical axes; the gate requires
// full V-filtered cross occupancy over these.
type guardCell struct {
	regime, presence, tomb, seqFence, recency, changed, statusPair string
}

func (k coverageKey) guard() guardCell {
	return guardCell{k.regime, k.presence, k.tomb, k.seqFence, k.recency, k.changed, k.statusPair}
}

// coverageRecorder accumulates observed cells across generator tiers.
type coverageRecorder struct {
	mu       sync.Mutex
	guards   map[guardCell]int
	marginal map[string]int // "axis=value" → count
	full     map[coverageKey]int
}

func newCoverageRecorder() *coverageRecorder {
	return &coverageRecorder{
		guards:   map[guardCell]int{},
		marginal: map[string]int{},
		full:     map[coverageKey]int{},
	}
}

func (r *coverageRecorder) record(k coverageKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guards[k.guard()]++
	r.full[k]++
	r.marginal[fmt.Sprintf("dirty=%v", k.dirty)]++
	r.marginal["depMapCell="+k.depMapCell]++
	r.marginal["fieldDeps="+k.fieldDeps]++
	r.marginal["cachedDeps="+k.cachedDeps]++
	r.marginal[fmt.Sprintf("useFreshDeps=%v", k.useFreshDeps)]++
	r.marginal[fmt.Sprintf("backingIsBd=%v", k.backingIsBd)]++
	r.marginal[fmt.Sprintf("confirmedClosed=%v", k.confirmedClosed)]++
	r.marginal["preserveOutcome="+k.preserveOutcome]++
	r.marginal["recency="+k.recency]++
	r.marginal["changed="+k.changed]++
	r.marginal["tomb="+k.tomb]++
	r.marginal["seqFence="+k.seqFence]++
	r.marginal["presence="+k.presence]++
	r.marginal["regime="+k.regime]++
}

// classifyRow derives the coverageKey for id from the INPUT (st, in). The
// id universe callers use is the union of all six maps plus freshByID, so
// deps-only orphans classify too.
func classifyRow(st storeState, in snapshotInputs, id string) coverageKey {
	k := coverageKey{}
	if in.quiescent(st) {
		k.regime = "quiescent"
	} else {
		k.regime = "mutated"
	}
	fresh, f := in.freshByID[id]
	cached, c := st.beads[id]
	switch {
	case f && c:
		k.presence = "both"
	case f:
		k.presence = "snap"
	case c:
		k.presence = "cache"
	default:
		k.presence = "neither"
	}
	k.tomb = fenceCell(st.deletedSeq, id, in.startSeq)
	k.seqFence = fenceCell(st.beadSeq, id, in.startSeq)
	k.recency = recencyCell(st.localBeadAt, id, in.now)

	postPreserve := computePostPreserveFresh(st, in)
	pf := postPreserve[id]
	switch {
	case f && c:
		k.changed = changedCell(cached, pf)
		k.statusPair = cached.Status + ">" + fresh.Status
	case f:
		k.changed = "na"
		k.statusPair = "?>" + fresh.Status
	case c:
		k.changed = "na"
		k.statusPair = cached.Status + ">?"
	default:
		k.changed = "na"
		k.statusPair = "na"
	}

	_, k.dirty = st.dirty[id]
	k.useFreshDeps = in.useFreshDeps
	k.backingIsBd = st.backingIsBd
	if in.useFreshDeps {
		k.depMapCell = depSliceCell(in.depMap, id)
		k.fieldDeps = "na"
	} else {
		k.depMapCell = "na"
		k.fieldDeps = fieldDepsCell(fresh, f)
	}
	k.cachedDeps = depSliceCell(st.deps, id)
	_, k.confirmedClosed = in.confirmedClosed[id]
	k.preserveOutcome = preserveOutcomeCell(st, in, id)
	return k
}

func fenceCell(m map[string]uint64, id string, startSeq uint64) string {
	v, ok := m[id]
	if !ok {
		return "none"
	}
	switch {
	case v < startSeq:
		return "lt"
	case v == startSeq:
		return "eq"
	default:
		return "gt"
	}
}

func recencyCell(m map[string]time.Time, id string, now time.Time) string {
	t, ok := m[id]
	if !ok || t.IsZero() {
		return "none"
	}
	d := now.Sub(t)
	switch {
	case d <= 0:
		return "now"
	case d <= 2500*millis:
		return "recent"
	case d <= 5000*millis:
		return "boundary"
	case d <= 5001*millis:
		return "justover"
	default:
		return "stale"
	}
}

const millis = 1000000 // time.Millisecond in ns as a bare constant for arithmetic

func changedCell(cached, fresh Bead) string {
	if !beadChanged(cached, fresh, true) {
		// Distinguish labels-only (skipLabels=true masks it) from truly equal.
		if !slicesEqualStr(cached.Labels, fresh.Labels) {
			return "labels"
		}
		return "equal"
	}
	switch {
	case cached.Status != fresh.Status:
		return "status"
	case !boolPtrEqual(cached.IsBlocked, fresh.IsBlocked):
		return "isblocked"
	case !mapsEqualStr(cached.Metadata, fresh.Metadata):
		return "metadata"
	case !slicesEqualStr(cached.Needs, fresh.Needs):
		return "needs"
	case !depsSliceEqual(cached.Dependencies, fresh.Dependencies):
		return "depsfield"
	default:
		return "other"
	}
}

func depSliceCell(m map[string][]Dep, id string) string {
	v, ok := m[id]
	if !ok {
		return "absent"
	}
	if v == nil {
		return "nil"
	}
	if len(v) == 0 {
		return "empty"
	}
	return "nonempty"
}

func fieldDepsCell(b Bead, present bool) string {
	if !present {
		return "na"
	}
	hasNeeds := len(b.Needs) > 0
	hasDeps := len(b.Dependencies) > 0
	switch {
	case hasNeeds && hasDeps:
		return "both"
	case hasNeeds:
		return "needs"
	case hasDeps:
		return "deps"
	default:
		return "none"
	}
}

// preserveOutcomeCell re-runs the preserve eligibility predicate on the INPUT
// state (council input-coverage hardening), using the shared helpers.
func preserveOutcomeCell(st storeState, in snapshotInputs, id string) string {
	item, ok := in.freshByID[id]
	if !ok {
		return "na"
	}
	if item.IsBlocked != nil {
		return "inapplicable"
	}
	cached, cok := st.beads[id]
	if !cok || cached.IsBlocked == nil {
		return "no-cached"
	}
	c, _ := newMergeHarnessStore(st)
	c.mu.Lock()
	defer c.mu.Unlock()
	freshDeps := c.depsForReconcileLocked(id, item, in.depMap, in.useFreshDeps)
	if depsChanged(c.deps[id], freshDeps) {
		return "blocked-deps"
	}
	if c.readyBlockingDependencyTargetStatusChangedLocked(freshDeps, in.freshByID) {
		return "blocked-target"
	}
	return "applied"
}

// --- small comparison helpers (test-local, avoid importing maps/slices) ---

func slicesEqualStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapsEqualStr(a, b StringMap) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func depsSliceEqual(a, b []Dep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rowIDUniverse is the maximal id set for classification/oracle iteration: the
// union of all six maps plus freshByID (council delete-B hardening — deps-only
// orphans must classify).
func rowIDUniverse(st storeState, in snapshotInputs) []string {
	set := map[string]struct{}{}
	for id := range st.beads {
		set[id] = struct{}{}
	}
	for id := range st.deps {
		set[id] = struct{}{}
	}
	for id := range st.dirty {
		set[id] = struct{}{}
	}
	for id := range st.beadSeq {
		set[id] = struct{}{}
	}
	for id := range st.localBeadAt {
		set[id] = struct{}{}
	}
	for id := range st.deletedSeq {
		set[id] = struct{}{}
	}
	for id := range in.freshByID {
		set[id] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
