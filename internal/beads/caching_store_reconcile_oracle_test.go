package beads

// The equivalence oracle for the reconcile differential gate.
//
// Design (proved in the plan §5 and re-derived here):
//   * The collapsed pipeline's absorb and eviction loops are line-for-line
//     transliterations of Branch A's loops, and the GC sweep emits no
//     notifications and touches no counters. Therefore NEW's notification
//     multiset, add/remove/update counters, and every seam-written scalar
//     (state, lastFreshAt, mutationSeq, primeErr, syncFailures, stats times)
//     are IDENTICAL to the reference branch on every input — no delta.
//   * Only the six per-row maps + depsComplete diverge, and only on the
//     exactly-enumerated §2 delta id-sets.
//
// The oracle builds the FULL expected NEW end-state from the reference
// end-state: scalars/notifications copied verbatim (asserting exact equality),
// the six maps + depsComplete transformed per an INDEPENDENT case-oracle that
// derives the §2 deltas from the INPUT state alone (it calls the shared pure
// helpers beadChanged/recentLocalMutation/preserve — pinned separately — but
// never reconcileMergeDecision or the merge pipeline). Assertion is exact
// reflect.DeepEqual: this pins apply(delta, refEnd) == newEnd bidirectionally,
// so both under-collection (a missed GC) and over-collection (a GC'd protected
// fence) fail. Spec-independent invariants on NEW-end alone add redundant
// power against a matrix misread.

import (
	"sort"
	"testing"
	"time"
)

// perIDView is the full per-id slice of the six maps, with presence tracked
// separately from value so nil-vs-empty and zero-vs-absent are distinguished.
type perIDView struct {
	hasBead    bool
	bead       Bead
	hasDeps    bool
	deps       []Dep
	dirty      bool
	hasBeadSeq bool
	beadSeq    uint64
	hasLocalAt bool
	localAt    time.Time
	hasDeleted bool
	deletedSeq uint64
}

func viewOf(end mergeEndState, id string) perIDView {
	v := perIDView{}
	v.bead, v.hasBead = end.beads[id]
	v.deps, v.hasDeps = end.deps[id]
	_, v.dirty = end.dirty[id]
	v.beadSeq, v.hasBeadSeq = end.beadSeq[id]
	v.localAt, v.hasLocalAt = end.localBeadAt[id]
	v.deletedSeq, v.hasDeleted = end.deletedSeq[id]
	return v
}

func viewOfState(st storeState, id string) perIDView {
	v := perIDView{}
	v.bead, v.hasBead = st.beads[id]
	v.deps, v.hasDeps = st.deps[id]
	_, v.dirty = st.dirty[id]
	v.beadSeq, v.hasBeadSeq = st.beadSeq[id]
	v.localAt, v.hasLocalAt = st.localBeadAt[id]
	v.deletedSeq, v.hasDeleted = st.deletedSeq[id]
	return v
}

// deltaKind is the §2 delta an id exhibits (or none).
type deltaKind int

const (
	deltaNone           deltaKind = iota
	deltaGCOrphan                 // D1/D3 (mutated): stale unprotected orphan the sweep collects
	deltaD4RecencyKept            // quiescent absorb recencyKeep: NEW keeps cached deps
	deltaD5RecentAbsorb           // quiescent absorb, recent, no recencyKeep: NEW keeps fences
	deltaD1RecentOrphan           // quiescent orphan with recent localAt: NEW keeps everything
)

// classifyDelta derives the delta for id from the INPUT (st, in) and the
// post-preserve fresh view. It is written from the §2 matrix and shares no
// code with reconcileMergeDecision or mergeSnapshotLocked.
func classifyDelta(st storeState, in snapshotInputs, postPreserveFresh map[string]Bead, refEnd mergeEndState, id string) deltaKind {
	if in.quiescent(st) {
		freshBead, f := in.freshByID[id]
		_ = freshBead
		cached, c := st.beads[id]
		recent := recentLocalMutation(st.localBeadAt[id], in.now)
		switch {
		case f:
			recencyKeep := c && recent && beadChanged(cached, postPreserveFresh[id], true)
			switch {
			case recencyKeep:
				return deltaD4RecencyKept
			case recent:
				return deltaD5RecentAbsorb
			default:
				return deltaNone
			}
		case c:
			return deltaNone // eviction cell never diverges from B
		default:
			// orphan (no row either side). Quiescent ⇒ fences <= startSeq
			// (invariant Q), so the only protector is recency.
			if recent {
				return deltaD1RecentOrphan
			}
			return deltaNone
		}
	}
	// Mutated regime, reference = Branch A end-state. NEW = A + GC sweep.
	// Divergence is exactly the orphan ids the sweep collects (D1/D3).
	if _, inBeads := refEnd.beads[id]; inBeads {
		return deltaNone
	}
	if _, inFresh := in.freshByID[id]; inFresh {
		return deltaNone
	}
	if !stateHasAnyOrphanEntry(refEnd, id) {
		return deltaNone
	}
	// Protector check against A-end values (the sweep runs on post-loop state).
	if refEnd.deletedSeq[id] > in.startSeq || refEnd.beadSeq[id] > in.startSeq {
		return deltaNone
	}
	if recentLocalMutation(refEnd.localBeadAt[id], in.now) {
		return deltaNone
	}
	return deltaGCOrphan
}

func stateHasAnyOrphanEntry(end mergeEndState, id string) bool {
	if _, ok := end.deletedSeq[id]; ok {
		return true
	}
	if _, ok := end.dirty[id]; ok {
		return true
	}
	if _, ok := end.beadSeq[id]; ok {
		return true
	}
	if _, ok := end.localBeadAt[id]; ok {
		return true
	}
	if _, ok := end.deps[id]; ok {
		return true
	}
	return false
}

// expectedNewView returns the per-id view NEW must produce, transforming the
// reference view per the classified delta.
func expectedNewView(st storeState, _ snapshotInputs, refEnd mergeEndState, id string, kind deltaKind) perIDView {
	base := viewOf(refEnd, id)
	switch kind {
	case deltaNone:
		return base
	case deltaGCOrphan:
		return perIDView{} // fully collected
	case deltaD4RecencyKept:
		// NEW leaves the cached deps in place instead of installing fresh deps.
		base.deps, base.hasDeps = st.deps[id]
		return base
	case deltaD5RecentAbsorb:
		// seqClearGuarded keeps the input beadSeq/localBeadAt through the window.
		base.beadSeq, base.hasBeadSeq = st.beadSeq[id]
		base.localAt, base.hasLocalAt = st.localBeadAt[id]
		return base
	case deltaD1RecentOrphan:
		// NEW keeps every input orphan entry; B wiped them.
		return viewOfState(st, id)
	default:
		return base
	}
}

// buildExpectedNewEnd assembles the full expected NEW end-state from the
// reference end-state and the per-id case-oracle.
func buildExpectedNewEnd(st storeState, in snapshotInputs, postPreserveFresh map[string]Bead, refEnd mergeEndState) mergeEndState {
	exp := refEnd // copies scalars verbatim — asserts exact scalar equality
	exp.beads = map[string]Bead{}
	exp.deps = map[string][]Dep{}
	exp.dirty = map[string]struct{}{}
	exp.beadSeq = map[string]uint64{}
	exp.localBeadAt = map[string]time.Time{}
	exp.deletedSeq = map[string]uint64{}

	for id := range allOracleIDs(st, in, refEnd) {
		kind := classifyDelta(st, in, postPreserveFresh, refEnd, id)
		v := expectedNewView(st, in, refEnd, id, kind)
		if v.hasBead {
			exp.beads[id] = v.bead
		}
		if v.hasDeps {
			exp.deps[id] = v.deps
		}
		if v.dirty {
			exp.dirty[id] = struct{}{}
		}
		if v.hasBeadSeq {
			exp.beadSeq[id] = v.beadSeq
		}
		if v.hasLocalAt {
			exp.localBeadAt[id] = v.localAt
		}
		if v.hasDeleted {
			exp.deletedSeq[id] = v.deletedSeq
		}
	}
	// depsComplete is regime-uniform in the collapsed seam: reconcileMergeDecision
	// has no regime concept, so the flag is a single fold — useFreshDeps, dropped
	// to false the moment any absorb-cell skip leaves the cached deps map an
	// unfaithful projection of the fresh scan. Re-derived here independently from
	// the input state and the shared pure helpers (never reconcileMergeDecision).
	// Without the D4 divergent-deps term this reproduces refEnd.depsComplete for
	// BOTH frozen branches exactly; the term adds the degradation the collapse
	// deliberately introduces so a recency-keep can no longer serve stale cached
	// deps under depsComplete=true.
	exp.depsComplete = expectedNextDepsComplete(st, in, postPreserveFresh)
	return exp
}

// expectedNextDepsComplete independently reproduces the seam's nextDepsComplete
// fold: it starts at useFreshDeps and drops to false on any absorb-cell (fresh
// present) skip over a cached row that leaves a deps hole — a fence or recency
// skip whose row has no cached deps entry, or a recency-keep that retains cached
// deps diverging from the fresh snapshot. Fence beats recency, matching the
// decision's arm ordering. Derived from input state + shared pure helpers only.
func expectedNextDepsComplete(st storeState, in snapshotInputs, postPreserveFresh map[string]Bead) bool {
	freshDepsByID := computeFreshDepsByID(st, in, postPreserveFresh)
	complete := in.useFreshDeps
	for id, fresh := range postPreserveFresh {
		cached, cachedExists := st.beads[id]
		if !cachedExists {
			continue // created row: no skip, no degradation
		}
		cachedDeps, hasCachedDeps := st.deps[id]
		switch {
		case st.deletedSeq[id] > in.startSeq || st.beadSeq[id] > in.startSeq:
			if !hasCachedDeps {
				complete = false
			}
		case recentLocalMutation(st.localBeadAt[id], in.now) && beadChanged(cached, fresh, true):
			if !hasCachedDeps || depsChanged(cachedDeps, freshDepsByID[id]) {
				complete = false
			}
		}
	}
	return complete
}

// computeFreshDepsByID returns, per fresh row, the deps depsForReconcileLocked
// would compute — the identical fresh-deps input all three implementations feed
// their skip/absorb arms. A pre-merge harness store gives depsForReconcileLocked
// the same cached-deps view (and BdStore vs mem fallback) the live seam reads.
func computeFreshDepsByID(st storeState, in snapshotInputs, postPreserveFresh map[string]Bead) map[string][]Dep {
	c, _ := newMergeHarnessStore(st)
	out := make(map[string][]Dep, len(postPreserveFresh))
	c.mu.Lock()
	for id, fresh := range postPreserveFresh {
		out[id] = c.depsForReconcileLocked(id, fresh, in.depMap, in.useFreshDeps)
	}
	c.mu.Unlock()
	return out
}

// allOracleIDs is the id universe the oracle must decide: every id referenced
// by the reference end-state, the input state, or the snapshot.
func allOracleIDs(st storeState, in snapshotInputs, refEnd mergeEndState) map[string]struct{} {
	ids := map[string]struct{}{}
	add := func(id string) { ids[id] = struct{}{} }
	for id := range refEnd.beads {
		add(id)
	}
	for id := range refEnd.deps {
		add(id)
	}
	for id := range refEnd.dirty {
		add(id)
	}
	for id := range refEnd.beadSeq {
		add(id)
	}
	for id := range refEnd.localBeadAt {
		add(id)
	}
	for id := range refEnd.deletedSeq {
		add(id)
	}
	for id := range st.beads {
		add(id)
	}
	for id := range st.deps {
		add(id)
	}
	for id := range st.dirty {
		add(id)
	}
	for id := range st.beadSeq {
		add(id)
	}
	for id := range st.localBeadAt {
		add(id)
	}
	for id := range st.deletedSeq {
		add(id)
	}
	for id := range in.freshByID {
		add(id)
	}
	return ids
}

// computePostPreserveFresh returns freshByID after the (shared, unchanged)
// preserve pass, so the case-oracle sees the exact fresh beads all three
// implementations see.
func computePostPreserveFresh(st storeState, in snapshotInputs) map[string]Bead {
	c, _ := newMergeHarnessStore(st)
	fresh := cloneBeadMap(in.freshByID)
	c.mu.Lock()
	c.preserveCachedReadyProjectionLocked(fresh, in.depMap, in.useFreshDeps)
	c.mu.Unlock()
	return fresh
}

// ---------------------------------------------------------------------------
// Notification multiset comparison
// ---------------------------------------------------------------------------

func assertNotificationsEqual(t *testing.T, name string, want, got []cacheNotification) {
	t.Helper()
	assertPerIDUnique(t, name+" (ref)", want)
	assertPerIDUnique(t, name+" (new)", got)
	ws := sortNotifications(want)
	gs := sortNotifications(got)
	if len(ws) != len(gs) {
		t.Fatalf("%s: notification count ref=%d new=%d\nref=%v\nnew=%v", name, len(ws), len(gs), ws, gs)
	}
	for i := range ws {
		if ws[i].eventType != gs[i].eventType || !beadsIdentical(ws[i].bead, gs[i].bead) {
			t.Fatalf("%s: notification[%d] ref={%s %s} new={%s %s} (payload differs=%v)",
				name, i, ws[i].eventType, ws[i].bead.ID, gs[i].eventType, gs[i].bead.ID,
				!beadsIdentical(ws[i].bead, gs[i].bead))
		}
	}
}

func assertPerIDUnique(t *testing.T, name string, ns []cacheNotification) {
	t.Helper()
	seen := map[string]struct{}{}
	for _, n := range ns {
		if _, dup := seen[n.bead.ID]; dup {
			t.Fatalf("%s: per-id notification uniqueness broken for %q — the multiset comparison assumption is invalid", name, n.bead.ID)
		}
		seen[n.bead.ID] = struct{}{}
	}
}

func sortNotifications(ns []cacheNotification) []cacheNotification {
	out := make([]cacheNotification, len(ns))
	copy(out, ns)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].bead.ID != out[j].bead.ID {
			return out[i].bead.ID < out[j].bead.ID
		}
		return out[i].eventType < out[j].eventType
	})
	return out
}

// beadsIdentical is exact struct equality (reflect.DeepEqual), NOT beadChanged
// — a lost Labels slice or metadata entry must fail even where skipLabels-blind
// comparison would pass it.
func beadsIdentical(a, b Bead) bool {
	return reflectDeepEqual(a, b)
}

// ---------------------------------------------------------------------------
// Spec-independent NEW-end invariants (redundant power vs a matrix misread)
// ---------------------------------------------------------------------------

func assertNewEndInvariants(t *testing.T, name string, end mergeEndState, in snapshotInputs) {
	t.Helper()
	// INV1 (no leaks): every orphan id (no bead) carrying any fence/deps entry
	// must have a live protector — a fence > startSeq or a recent localAt.
	orphanIDs := map[string]struct{}{}
	for id := range end.deletedSeq {
		orphanIDs[id] = struct{}{}
	}
	for id := range end.dirty {
		orphanIDs[id] = struct{}{}
	}
	for id := range end.beadSeq {
		orphanIDs[id] = struct{}{}
	}
	for id := range end.localBeadAt {
		orphanIDs[id] = struct{}{}
	}
	for id := range end.deps {
		orphanIDs[id] = struct{}{}
	}
	for id := range orphanIDs {
		if _, hasBead := end.beads[id]; hasBead {
			continue
		}
		protected := end.deletedSeq[id] > in.startSeq ||
			end.beadSeq[id] > in.startSeq ||
			recentLocalMutation(end.localBeadAt[id], in.now)
		if !protected {
			t.Fatalf("%s: INV1 leak — orphan %q retained a fence/deps entry with no protector (deletedSeq=%d beadSeq=%d localAt=%v startSeq=%d)",
				name, id, end.deletedSeq[id], end.beadSeq[id], end.localBeadAt[id], in.startSeq)
		}
	}
	// INV2 (V1): deletedSeq present ⇒ beads absent.
	for id := range end.deletedSeq {
		if _, ok := end.beads[id]; ok {
			t.Fatalf("%s: INV2 violated — %q has both a live row and a tombstone", name, id)
		}
	}
	// INV3: the sentinel convention forbids a zero-valued fence entry.
	for id, v := range end.beadSeq {
		if v == 0 {
			t.Fatalf("%s: INV3 violated — beadSeq[%q]==0 (zero means absent by convention)", name, id)
		}
	}
	for id, v := range end.deletedSeq {
		if v == 0 {
			t.Fatalf("%s: INV3 violated — deletedSeq[%q]==0", name, id)
		}
	}
}

// ---------------------------------------------------------------------------
// The differential assertion
// ---------------------------------------------------------------------------

// assertDifferential runs the frozen reference branch (selected by regime) and
// the live collapsed seam on byte-identical clones of (st, in), and asserts
// full end-state + notification equivalence modulo the §2 deltas. Each
// implementation is run twice on fresh clones (Go randomizes map iteration per
// range) to detect any order dependence before cross-comparing — an
// order-dependent divergence would otherwise surface as an unreproducible CI
// flake.
func assertDifferential(t *testing.T, name string, st storeState, in snapshotInputs) {
	t.Helper()

	// Normalize inputs exactly as the seam does before any implementation or the
	// oracle reads them. cloneStoreState/cloneSnapshotInputs run cloneDeps over
	// every deps entry, collapsing an empty-non-nil []Dep{} to nil just as the
	// live seam stores it. The three impl runs already clone internally, but the
	// case-oracle reads st directly (viewOfState/expectedNewView); without this
	// the oracle would expect a raw []Dep{} on a recency-kept orphan while the
	// seam produced nil — a harness-only false divergence (regression-pinned by
	// the FuzzReconcileMergeDifferential seed).
	st = cloneStoreState(st)
	in = cloneSnapshotInputs(in)

	// Determinism self-check per implementation.
	newRes := runNewMerge(cloneStoreState(st), cloneSnapshotInputs(in))
	newRes2 := runNewMerge(cloneStoreState(st), cloneSnapshotInputs(in))
	assertSelfDeterministic(t, name+" NEW", newRes, newRes2)

	var ref mergeImplResult
	if in.quiescent(st) {
		ref = runLegacyB(cloneStoreState(st), cloneSnapshotInputs(in))
		ref2 := runLegacyB(cloneStoreState(st), cloneSnapshotInputs(in))
		assertSelfDeterministic(t, name+" legacyB", ref, ref2)
	} else {
		ref = runLegacyA(cloneStoreState(st), cloneSnapshotInputs(in))
		ref2 := runLegacyA(cloneStoreState(st), cloneSnapshotInputs(in))
		assertSelfDeterministic(t, name+" legacyA", ref, ref2)
	}

	// Merge purity: zero backing I/O inside the seam (off-BdStore path).
	if !st.backingIsBd {
		if newRes.backingCalls != 0 {
			t.Fatalf("%s: NEW made %d backing calls during merge — the seam must be I/O-free", name, newRes.backingCalls)
		}
		if ref.backingCalls != 0 {
			t.Fatalf("%s: reference made %d backing calls during merge", name, ref.backingCalls)
		}
	}

	// Notifications and counters must be EXACTLY equal (no delta by analysis).
	assertNotificationsEqual(t, name, ref.notifications, newRes.notifications)

	// Six-map + depsComplete: build the full expected NEW end-state from the
	// reference and assert exact equality (scalars/counters copied ⇒ asserted
	// equal; maps transformed per the independent case-oracle).
	postPreserve := computePostPreserveFresh(st, in)
	exp := buildExpectedNewEnd(st, in, postPreserve, ref.end)
	if !endStatesEqual(exp, newRes.end) {
		t.Fatalf("%s: end-state divergence\n%s", name, diffEndStates(exp, newRes.end))
	}

	// Spec-independent invariants on the NEW end-state alone.
	assertNewEndInvariants(t, name, newRes.end, in)
}

func assertSelfDeterministic(t *testing.T, name string, a, b mergeImplResult) {
	t.Helper()
	if !endStatesEqual(a.end, b.end) {
		t.Fatalf("%s: NON-DETERMINISTIC end-state across two runs (map-iteration order dependence)\n%s",
			name, diffEndStates(a.end, b.end))
	}
	assertNotificationsEqual(t, name+" self-determinism", a.notifications, b.notifications)
}
