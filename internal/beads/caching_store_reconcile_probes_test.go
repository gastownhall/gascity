package beads

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Oracle teeth: prove the gate is not vacuous
// ---------------------------------------------------------------------------

// TestReconcileMergeDeltas_AreReal asserts that every fixture named for a §2
// delta actually produces a NEW end-state that DIFFERS from the reference
// branch — i.e. the delta is a genuine behavioral change the oracle is
// characterizing, not rubber-stamped equality. assertDifferential still passes
// on these (proving the difference is exactly the enumerated delta).
func TestReconcileMergeDeltas_AreReal(t *testing.T) {
	// Markers for fixtures that are GENUINE deltas vs the reference branch.
	// Quiescent stale-orphan GC cases are ≡ to Branch B (both wipe), so they are
	// deliberately NOT listed here.
	deltaMarkers := []string{"D5", "D4", "D2", "D3prime", "D1prime", "recent_kept", "orphan_gc", "fences_gc"}
	var checked int
	for _, f := range mergeFixtures() {
		isDelta := false
		for _, m := range deltaMarkers {
			if strings.Contains(f.name, m) {
				isDelta = true
				break
			}
		}
		if !isDelta {
			continue
		}
		st := cloneStoreState(f.st)
		in := cloneSnapshotInputs(f.in)
		var ref mergeImplResult
		if in.quiescent(st) {
			ref = runLegacyB(cloneStoreState(st), cloneSnapshotInputs(in))
		} else {
			ref = runLegacyA(cloneStoreState(st), cloneSnapshotInputs(in))
		}
		newRes := runNewMerge(cloneStoreState(st), cloneSnapshotInputs(in))
		if endStatesEqual(ref.end, newRes.end) {
			t.Errorf("%s: labeled a delta but NEW == reference (delta is vacuous)", f.name)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no delta fixtures found — teeth test is inert")
	}
}

// TestReconcileMergeOracleHasTeeth proves the comparison detects a corrupted
// end-state, guarding against a vacuous reflect.DeepEqual.
func TestReconcileMergeOracleHasTeeth(t *testing.T) {
	f := mergeFixtures()[0]
	newRes := runNewMerge(cloneStoreState(f.st), cloneSnapshotInputs(f.in))
	corrupt := newRes.end
	corrupt.beads = cloneBeadMap(newRes.end.beads)
	corrupt.beads["injected-ghost"] = bead("injected-ghost", "open")
	if endStatesEqual(newRes.end, corrupt) {
		t.Fatal("oracle failed to detect an injected ghost row")
	}
	// A one-field scalar change must also be caught.
	corrupt2 := newRes.end
	corrupt2.statsAdds++
	if endStatesEqual(newRes.end, corrupt2) {
		t.Fatal("oracle failed to detect a stats.Adds drift")
	}
}

// ---------------------------------------------------------------------------
// Read-projection probe (plan §5.1(5))
// ---------------------------------------------------------------------------

// mountEndState builds a live cacheLive store from an end-state over a MemStore
// backing seeded with the "reality" rows (the active beads bd would return).
func mountEndState(end mergeEndState, truth *MemStore) *CachingStore {
	c := &CachingStore{
		backing:      truth,
		beads:        cloneBeadMap(end.beads),
		deps:         cloneDepMap(end.deps),
		depsComplete: end.depsComplete,
		dirty:        cloneDirty(end.dirty),
		beadSeq:      cloneU64Map(end.beadSeq),
		localBeadAt:  cloneTimeMap(end.localBeadAt),
		deletedSeq:   cloneU64Map(end.deletedSeq),
		state:        cacheLive,
	}
	ensureMaps(c)
	return c
}

// TestReconcileMergeReadProjection mounts the reference and NEW end-states over
// an identical backing that reflects reality (freshByID rows exist; orphan and
// deleted ids do not) and asserts Get and CachedReady serve identically (or that
// NEW only ever moves in the safe direction). Map equality already implies read
// equality on non-delta cells; this probe is the check that the §2 delta
// divergences are invisible at the read surface — D1's tombstone GC falls
// through to a backing that also says not-found, etc. The dependency-list read
// surface is covered separately by TestReconcileMergeD4RecencyKeepDepListContract,
// which exercises the one delta (D4) that touches deps.
func TestReconcileMergeReadProjection(t *testing.T) {
	states := append(mergeFixtures(), genGridStates()...)
	states = append(states, genSeededStates(3, 400)...)
	for _, f := range states {
		f := f
		st := cloneStoreState(f.st)
		in := cloneSnapshotInputs(f.in)

		var ref mergeImplResult
		if in.quiescent(st) {
			ref = runLegacyB(cloneStoreState(st), cloneSnapshotInputs(in))
		} else {
			ref = runLegacyA(cloneStoreState(st), cloneSnapshotInputs(in))
		}
		newRes := runNewMerge(cloneStoreState(st), cloneSnapshotInputs(in))

		// Backing truth: the active beads a full scan returned this cycle.
		truthRef := NewMemStore()
		truthNew := NewMemStore()
		var truthRows []Bead
		for _, b := range in.freshByID {
			truthRows = append(truthRows, cloneBead(b))
		}
		seedMem(truthRef, truthRows)
		seedMem(truthNew, truthRows)

		cRef := mountEndState(ref.end, truthRef)
		cNew := mountEndState(newRes.end, truthNew)

		ids := rowIDUniverse(st, in)
		ids = append(ids, "never-seen-id")
		for _, id := range ids {
			bRef, eRef := cRef.Get(id)
			bNew, eNew := cNew.Get(id)
			if !sameGetResult(bRef, eRef, bNew, eNew) {
				t.Fatalf("%s: Get(%q) read-projection divergence ref=(%v,%v) new=(%v,%v)",
					f.name, id, bRef.ID, eRef, bNew.ID, eNew)
			}
		}

		rRef, okRef := cRef.CachedReady()
		rNew, okNew := cNew.CachedReady()
		switch {
		case okRef && okNew:
			// Both serve from cache ⇒ identical served set required (beads maps
			// are equal on non-GC'd ids, so served readiness must match).
			if !sameBeadSet(rRef, rNew) {
				t.Fatalf("%s: CachedReady served set differs", f.name)
			}
		case in.quiescent(st):
			// Reference = Branch B. The quiescent deltas (D2 depsComplete, D1'/D3'
			// a kept orphan dirty/fence) can only make NEW MORE conservative:
			// NEW may decline where B served, never the reverse.
			if okNew && !okRef {
				t.Fatalf("%s: CachedReady served from NEW but declined on Branch B — unsafe direction", f.name)
			}
		default:
			// Mutated regime, reference = Branch A. The D1/D3 orphan GC REMOVES a
			// leaked dirty/fence that made A decline permanently; NEW may serve
			// where A declined (the intended fix), never decline where A served.
			if okRef && !okNew {
				t.Fatalf("%s: CachedReady declined on NEW but Branch A served — a serving regression", f.name)
			}
		}
	}
}

// TestReconcileMergeD4RecencyKeepDepListContract pins the dependency-read
// contract for the D4 cell — a quiescent recency-keep whose retained cached deps
// diverge from the fresh full-scan snapshot. The collapse keeps the local deps
// (they may reflect an in-flight local write the snapshot lags — see the
// reg_2210 fixture) but must not then advertise the deps map as a complete,
// faithful projection of the scan. This closes the attempt-2 gap: the general
// read-projection probe exercised only Get and CachedReady, leaving the deps
// read surface for this delta unproven.
func TestReconcileMergeD4RecencyKeepDepListContract(t *testing.T) {
	const startSeq = uint64(100)
	// Quiescent recency-keep: the cached row is recent and body-changed vs the
	// fresh row, and its cached deps ([cached]) differ from the fresh snapshot
	// deps ([fresh]).
	st := storeState{
		beads:       map[string]Bead{"a": bead("a", "open")},
		deps:        map[string][]Dep{"a": {dep("a", "cached")}},
		beadSeq:     map[string]uint64{"a": 90},
		localBeadAt: map[string]time.Time{"a": fxRecent()},
		mutationSeq: startSeq,
	}
	in := snapshotInputs{
		freshByID:    map[string]Bead{"a": beadWith("a", "closed", func(_ *Bead) {})},
		depMap:       map[string][]Dep{"a": {dep("a", "fresh")}},
		useFreshDeps: true,
		startSeq:     startSeq,
		now:          fxNow,
	}
	newRes := runNewMerge(cloneStoreState(st), cloneSnapshotInputs(in))

	// The fix: a divergent recency-keep degrades depsComplete instead of
	// over-claiming a complete deps projection.
	if newRes.end.depsComplete {
		t.Fatal("D4 divergent recency-keep left depsComplete=true — the cache over-claims a complete deps projection")
	}
	// The retained local deps stay in the cache (the collapse keeps them where
	// Branch B overwrote them with the snapshot deps).
	if depsChanged(newRes.end.deps["a"], []Dep{dep("a", "cached")}) {
		t.Fatalf("D4 recency-keep should retain cached deps, got %v", newRes.end.deps["a"])
	}

	// Mount the NEW end-state over a backing whose DepList is the authoritative
	// answer, distinct from both the cached and the snapshot deps, so a fallback
	// is observable.
	truth := NewMemStore()
	truth.deps = []Dep{dep("a", "authoritative")}
	c := mountEndState(newRes.end, truth)

	// The public DepList reader fails closed to the backing: with depsComplete
	// degraded it must NOT serve the retained (possibly stale) cached deps as an
	// authoritative, complete projection.
	got, err := c.DepList("a", "down")
	if err != nil {
		t.Fatalf("DepList returned error: %v", err)
	}
	if depsChanged(got, []Dep{dep("a", "authoritative")}) {
		t.Fatalf("DepList must fall back to the backing when depsComplete is degraded; got %v (cached deps leaked to an authoritative read)", got)
	}

	// The cache-only reader still surfaces the retained local deps — the same
	// local-truth view cachedGetOnly serves for the retained bead body. It is the
	// explicit best-effort cache surface, not the authoritative projection.
	cached, err := c.cachedDepListOnly("a", "down")
	if err != nil {
		t.Fatalf("cachedDepListOnly returned error: %v", err)
	}
	if depsChanged(cached, []Dep{dep("a", "cached")}) {
		t.Fatalf("cachedDepListOnly should surface the retained local deps, got %v", cached)
	}
}

func seedMem(m *MemStore, rows []Bead) {
	for _, r := range rows {
		m.beads = append(m.beads, cloneBead(r))
	}
}

func sameGetResult(bRef Bead, eRef error, bNew Bead, eNew error) bool {
	refNF := errors.Is(eRef, ErrNotFound)
	newNF := errors.Is(eNew, ErrNotFound)
	if refNF || newNF {
		return refNF && newNF
	}
	if (eRef == nil) != (eNew == nil) {
		return false
	}
	if eRef != nil {
		return eRef.Error() == eNew.Error()
	}
	return beadsIdentical(bRef, bNew)
}

func sameBeadSet(a, b []Bead) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]Bead{}
	for _, x := range a {
		seen[x.ID] = x
	}
	for _, y := range b {
		x, ok := seen[y.ID]
		if !ok || !beadsIdentical(x, y) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Aliasing probe (plan §5.1 hardening): equal-clone and shared-backing compare
// identical, so scribble on every input/notification reference field after the
// merge and assert the store's maps do not move — pins today's clone discipline.
// ---------------------------------------------------------------------------

func TestReconcileMergeNoInputAliasing(t *testing.T) {
	for _, f := range mergeFixtures() {
		f := f
		st := cloneStoreState(f.st)
		in := cloneSnapshotInputs(f.in)

		c, _ := newMergeHarnessStore(st)
		c.mu.Lock()
		res := c.mergeSnapshotLocked(in.freshByID, in.confirmedClosed, in.depMap, in.useFreshDeps, in.startSeq, in.now)
		snapshot := captureEndState(c)
		c.mu.Unlock()

		// Scribble on every reference field of the inputs and notifications.
		scribbleBeadMap(in.freshByID)
		scribbleBeadMap(in.confirmedClosed)
		for k, v := range in.depMap {
			for i := range v {
				v[i] = dep("SCRIBBLE", "SCRIBBLE")
			}
			in.depMap[k] = append(v, dep("EXTRA", "EXTRA"))
		}
		for i := range res.notifications {
			res.notifications[i].bead.Labels = []string{"SCRIBBLE"}
			res.notifications[i].bead.Metadata = StringMap{"SCRIBBLE": "1"}
			if res.notifications[i].bead.Dependencies != nil {
				for j := range res.notifications[i].bead.Dependencies {
					res.notifications[i].bead.Dependencies[j] = dep("SCRIBBLE", "SCRIBBLE")
				}
			}
		}

		c.mu.Lock()
		after := captureEndState(c)
		c.mu.Unlock()
		if !endStatesEqual(snapshot, after) {
			t.Fatalf("%s: store mutated after scribbling on inputs/notifications — aliasing leak\n%s",
				f.name, diffEndStates(snapshot, after))
		}
	}
}

func scribbleBeadMap(m map[string]Bead) {
	for k, b := range m {
		b.Labels = []string{"SCRIBBLE"}
		b.Metadata = StringMap{"SCRIBBLE": "1"}
		b.Needs = []string{"SCRIBBLE"}
		b.Dependencies = []Dep{dep("SCRIBBLE", "SCRIBBLE")}
		m[k] = b
	}
}

// ---------------------------------------------------------------------------
// Fuzz tier
// ---------------------------------------------------------------------------

type byteCursor struct {
	data []byte
	pos  int
}

func (c *byteCursor) next() byte {
	if c.pos >= len(c.data) {
		return 0
	}
	b := c.data[c.pos]
	c.pos++
	return b
}

func (c *byteCursor) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(c.next()) % n
}

// decodeFuzzState interprets fuzz bytes as V-valid grid indices (rejection-free).
func decodeFuzzState(data []byte) (storeState, snapshotInputs) {
	cur := &byteCursor{data: data}
	const startSeq = uint64(100)
	mutated := cur.intn(2) == 0
	st := storeState{
		beads: map[string]Bead{}, deps: map[string][]Dep{}, dirty: map[string]struct{}{},
		beadSeq: map[string]uint64{}, localBeadAt: map[string]time.Time{}, deletedSeq: map[string]uint64{},
		backingIsBd: cur.intn(2) == 0,
	}
	in := snapshotInputs{
		freshByID: map[string]Bead{}, confirmedClosed: map[string]Bead{}, depMap: map[string][]Dep{},
		useFreshDeps: cur.intn(2) == 0, startSeq: startSeq, now: fxNow,
	}
	if mutated {
		st.mutationSeq = startSeq + uint64(1+cur.intn(80))
	} else {
		st.mutationSeq = startSeq
	}
	statuses := []string{"open", "in_progress", "closed"}
	recCells := []string{"none", "recent", "boundary", "justover", "stale", "now"}
	nRows := 1 + cur.intn(4)
	for r := 0; r < nRows; r++ {
		id := string(rune('a' + r))
		presence := cur.intn(4)
		cachedPresent := presence == 0 || presence == 2
		freshPresent := presence == 0 || presence == 1
		if cachedPresent {
			cb := bead(id, statuses[cur.intn(3)])
			fuzzFields(cur, &cb)
			st.beads[id] = cb
			if cur.intn(3) != 0 {
				st.deps[id] = fuzzDeps(cur, id)
			}
		}
		if freshPresent {
			fb := bead(id, statuses[cur.intn(3)])
			fuzzFields(cur, &fb)
			in.freshByID[id] = fb
			if in.useFreshDeps && cur.intn(3) != 0 {
				in.depMap[id] = fuzzDeps(cur, id)
			}
			if cachedPresent && st.beads[id].Status != "closed" && cur.intn(4) == 0 {
				in.confirmedClosed[id] = beadWith(id, "closed", func(_ *Bead) {})
			}
		}
		if cur.intn(2) == 0 {
			st.beadSeq[id] = randFenceFuzz(cur, startSeq, st.mutationSeq)
		}
		if !cachedPresent && cur.intn(2) == 0 {
			st.deletedSeq[id] = randFenceFuzz(cur, startSeq, st.mutationSeq)
		}
		if cur.intn(2) == 0 {
			if t, ok := recencyValue2(recCells[cur.intn(len(recCells))]); ok {
				st.localBeadAt[id] = t
			}
		}
		if cur.intn(3) == 0 {
			st.dirty[id] = struct{}{}
		}
		if presence == 3 && cur.intn(2) == 0 {
			st.deps[id] = fuzzDeps(cur, id)
		}
	}
	return st, in
}

func randFenceFuzz(cur *byteCursor, startSeq, mutationSeq uint64) uint64 {
	if mutationSeq > startSeq && cur.intn(2) == 0 {
		return startSeq + 1 + uint64(cur.intn(int(mutationSeq-startSeq)))
	}
	return uint64(1 + cur.intn(int(startSeq)))
}

func fuzzFields(cur *byteCursor, b *Bead) {
	if cur.intn(2) == 0 {
		b.Title = "t" + string(rune('0'+cur.intn(3)))
	}
	if cur.intn(3) == 0 {
		b.Labels = []string{"l" + string(rune('0'+cur.intn(2)))}
	}
	if cur.intn(3) == 0 {
		v := cur.intn(2) == 0
		b.IsBlocked = &v
	}
	if cur.intn(3) == 0 {
		b.Metadata = StringMap{"k": string(rune('0' + cur.intn(2)))}
	}
	if cur.intn(3) == 0 {
		b.Needs = []string{"n" + string(rune('0'+cur.intn(2)))}
	}
	if cur.intn(3) == 0 {
		b.Dependencies = []Dep{dep(b.ID, "d"+string(rune('0'+cur.intn(2))))}
	}
}

func fuzzDeps(cur *byteCursor, id string) []Dep {
	switch cur.intn(4) {
	case 0:
		return nil
	case 1:
		return []Dep{}
	default:
		n := 1 + cur.intn(2)
		ds := make([]Dep, n)
		for i := range ds {
			ds[i] = dep(id, "t"+string(rune('0'+cur.intn(3))))
		}
		return ds
	}
}

func FuzzReconcileMergeDifferential(f *testing.F) {
	// Seed the corpus from a few fixtures' byte-equivalents (arbitrary bytes
	// decode to V-valid states, so any seed is legal).
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{1, 1, 1, 0, 2, 2, 2, 3, 3, 3, 4, 4})
	f.Add([]byte{255, 254, 253, 200, 100, 50, 25, 12, 6, 3, 1})
	// Regression seed: decodes to a recent quiescent orphan (delta
	// D1RecentOrphan) carrying an empty-non-nil deps entry. Before the harness
	// normalized its inputs, the oracle read that raw []Dep{} while the seam
	// stored the cloneDeps-normalized nil, producing a false end-state
	// divergence. Pinned so the fuzz tier stays honest.
	f.Add([]byte("100071101001"))
	f.Fuzz(func(t *testing.T, data []byte) {
		st, in := decodeFuzzState(data)
		assertDifferential(t, "fuzz", st, in)
	})
}
