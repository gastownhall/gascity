package beads

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Tier 1: exhaustive guard-axis grid (single source of truth for the cross)
// ---------------------------------------------------------------------------

type guardCellSpec struct {
	regime       string
	presence     string
	tomb         string
	seq          string
	recency      string
	changed      string // "na" outside presence=both
	cachedStatus string
	freshStatus  string
}

var (
	changedVals      = []string{"equal", "status", "labels", "isblocked", "metadata", "needs", "depsfield"}
	cachedStatusVals = []string{"open", "in_progress", "closed"}
	freshStatusVals  = []string{"open", "closed"}
	recencyVals      = []string{"none", "recent", "boundary", "justover", "stale"}
)

func fenceValsFor(regime string) []string {
	if regime == "mutated" {
		return []string{"none", "lt", "eq", "gt"}
	}
	return []string{"none", "lt", "eq"}
}

// forEachGuardCell drives BOTH enumeration and generation so the intended
// cross and the generated corpus can never drift.
func forEachGuardCell(fn func(spec guardCellSpec)) {
	for _, regime := range []string{"quiescent", "mutated"} {
		fences := fenceValsFor(regime)
		for _, seq := range fences {
			for _, rec := range recencyVals {
				// presence = both (cached ⇒ tomb=none by V1)
				for _, cs := range cachedStatusVals {
					for _, ch := range changedVals {
						fn(guardCellSpec{regime, "both", "none", seq, rec, ch, cs, derivedFreshStatus(cs, ch)})
					}
				}
				// presence = cache (cached ⇒ tomb=none)
				for _, cs := range cachedStatusVals {
					fn(guardCellSpec{regime, "cache", "none", seq, rec, "na", cs, "na"})
				}
				// presence = snap (no cached bead ⇒ tomb may be set)
				for _, tomb := range fences {
					for _, fs := range freshStatusVals {
						fn(guardCellSpec{regime, "snap", tomb, seq, rec, "na", "na", fs})
					}
					// presence = neither (orphan fences/deps only)
					fn(guardCellSpec{regime, "neither", tomb, seq, rec, "na", "na", "na"})
				}
			}
		}
	}
}

func derivedFreshStatus(cached, changed string) string {
	if changed == "status" {
		if cached == "closed" {
			return "open"
		}
		return "closed"
	}
	return cached
}

func fenceValue(cell string) uint64 {
	switch cell {
	case "lt":
		return 90
	case "eq":
		return 100
	case "gt":
		return 150
	default:
		return 0
	}
}

func recencyValue(cell string) (time.Time, bool) {
	switch cell {
	case "recent":
		return fxRecent(), true
	case "boundary":
		return fxBoundary(), true
	case "justover":
		return fxJustOver(), true
	case "stale":
		return fxStale(), true
	default:
		return time.Time{}, false
	}
}

// buildGuardState materializes a single-row state for id "a" from a spec.
func buildGuardState(spec guardCellSpec) (storeState, snapshotInputs, string) {
	const startSeq = uint64(100)
	st := storeState{
		beads:       map[string]Bead{},
		deps:        map[string][]Dep{},
		dirty:       map[string]struct{}{},
		beadSeq:     map[string]uint64{},
		localBeadAt: map[string]time.Time{},
		deletedSeq:  map[string]uint64{},
	}
	in := snapshotInputs{
		freshByID:    map[string]Bead{},
		depMap:       map[string][]Dep{},
		useFreshDeps: true,
		startSeq:     startSeq,
		now:          fxNow,
	}
	if spec.regime == "mutated" {
		st.mutationSeq = 200
	} else {
		st.mutationSeq = startSeq
	}
	const id = "a"

	cachedPresent := spec.presence == "both" || spec.presence == "cache"
	freshPresent := spec.presence == "both" || spec.presence == "snap"

	var cached Bead
	if cachedPresent {
		cached = bead(id, spec.cachedStatus)
		st.beads[id] = cached
		st.deps[id] = []Dep{dep(id, "cacheddep")}
	}
	if freshPresent {
		var fresh Bead
		if spec.presence == "both" {
			fresh = deriveFresh(cached, spec.changed)
		} else {
			fresh = bead(id, spec.freshStatus)
		}
		in.freshByID[id] = fresh
		in.depMap[id] = []Dep{dep(id, "cacheddep")} // equal to cached ⇒ no depsChanged noise
	}

	// Fences / recency / tombstone.
	if v := fenceValue(spec.seq); v != 0 {
		st.beadSeq[id] = v
	}
	if !cachedPresent { // V1: no tombstone alongside a live row
		if v := fenceValue(spec.tomb); v != 0 {
			st.deletedSeq[id] = v
		}
	}
	if t, ok := recencyValue(spec.recency); ok {
		st.localBeadAt[id] = t
	}

	name := fmt.Sprintf("%s_%s_tomb-%s_seq-%s_rec-%s_ch-%s_cs-%s_fs-%s",
		spec.regime, spec.presence, spec.tomb, spec.seq, spec.recency, spec.changed, spec.cachedStatus, spec.freshStatus)
	return st, in, name
}

func deriveFresh(cached Bead, changed string) Bead {
	f := cloneBead(cached)
	switch changed {
	case "status":
		if cached.Status == "closed" {
			f.Status = "open"
		} else {
			f.Status = "closed"
		}
	case "labels":
		f.Labels = []string{"L1"}
	case "isblocked":
		v := true
		f.IsBlocked = &v
	case "metadata":
		f.Metadata = StringMap{"k": "v"}
	case "needs":
		f.Needs = []string{"n1"}
	case "depsfield":
		f.Dependencies = []Dep{dep(cached.ID, "d1")}
	case "equal":
		// identical
	}
	return f
}

func genGridStates() []mergeFixture {
	var out []mergeFixture
	forEachGuardCell(func(spec guardCellSpec) {
		st, in, name := buildGuardState(spec)
		out = append(out, mergeFixture{name: name, st: st, in: in})
	})
	return out
}

// ---------------------------------------------------------------------------
// Marginal generator: guarantees each soft/hardened axis value is observed
// ---------------------------------------------------------------------------

func genMarginalStates() []mergeFixture {
	var out []mergeFixture
	const q = uint64(100)
	base := func() (storeState, snapshotInputs) {
		return storeState{
				beads: map[string]Bead{}, deps: map[string][]Dep{}, dirty: map[string]struct{}{},
				beadSeq: map[string]uint64{}, localBeadAt: map[string]time.Time{}, deletedSeq: map[string]uint64{},
				mutationSeq: q,
			},
			snapshotInputs{freshByID: map[string]Bead{}, depMap: map[string][]Dep{}, useFreshDeps: true, startSeq: q, now: fxNow}
	}

	// dirty=true
	{
		st, in := base()
		st.beads["a"] = bead("a", "open")
		st.deps["a"] = []Dep{dep("a", "x")}
		st.dirty["a"] = struct{}{}
		in.freshByID["a"] = beadWith("a", "open", func(b *Bead) { b.Title = "chg" })
		in.depMap["a"] = []Dep{dep("a", "x")}
		out = append(out, mergeFixture{"marg_dirty_true", st, in})
	}
	// depMapCell nil / empty / nonempty (useFreshDeps=true)
	for _, dc := range []string{"nil", "empty", "nonempty"} {
		st, in := base()
		st.beads["a"] = bead("a", "open")
		st.deps["a"] = []Dep{dep("a", "x")}
		in.freshByID["a"] = bead("a", "open")
		switch dc {
		case "nil":
			in.depMap["a"] = nil
			in.depMap = map[string][]Dep{"a": nil}
		case "empty":
			in.depMap["a"] = []Dep{}
		case "nonempty":
			in.depMap["a"] = []Dep{dep("a", "x")}
		}
		out = append(out, mergeFixture{"marg_depMap_" + dc, st, in})
	}
	// fieldDeps none/needs/deps/both (useFreshDeps=false)
	for _, fd := range []string{"none", "needs", "deps", "both"} {
		st, in := base()
		in.useFreshDeps = false
		st.beads["a"] = bead("a", "open")
		fresh := bead("a", "open")
		switch fd {
		case "needs":
			fresh.Needs = []string{"n1"}
		case "deps":
			fresh.Dependencies = []Dep{dep("a", "d1")}
		case "both":
			fresh.Needs = []string{"n1"}
			fresh.Dependencies = []Dep{dep("a", "d1")}
		}
		in.freshByID["a"] = fresh
		out = append(out, mergeFixture{"marg_fieldDeps_" + fd, st, in})
	}
	// cachedDeps absent/nil/empty/nonempty
	for _, cd := range []string{"absent", "nil", "empty", "nonempty"} {
		st, in := base()
		st.beads["a"] = bead("a", "open")
		switch cd {
		case "nil":
			st.deps = map[string][]Dep{"a": nil}
		case "empty":
			st.deps["a"] = []Dep{}
		case "nonempty":
			st.deps["a"] = []Dep{dep("a", "x")}
		}
		in.freshByID["a"] = beadWith("a", "open", func(b *Bead) { b.Title = "chg" })
		in.depMap["a"] = []Dep{dep("a", "x")}
		out = append(out, mergeFixture{"marg_cachedDeps_" + cd, st, in})
	}
	// confirmedClosed=true (cache-only non-closed stale eviction)
	{
		st, in := base()
		st.beads["a"] = bead("a", "open")
		st.localBeadAt["a"] = fxStale()
		in.confirmedClosed = map[string]Bead{"a": beadWith("a", "closed", func(b *Bead) { b.Title = "auth" })}
		out = append(out, mergeFixture{"marg_confirmedClosed", st, in})
	}
	// recency "now" (zero elapsed) — council boundary cell
	{
		st, in := base()
		st.beads["a"] = bead("a", "open")
		st.localBeadAt["a"] = fxNow
		in.freshByID["a"] = beadWith("a", "closed", func(_ *Bead) {})
		in.depMap["a"] = []Dep{dep("a", "x")}
		out = append(out, mergeFixture{"marg_recency_now", st, in})
	}
	// preserveOutcome: inapplicable / no-cached / applied / blocked-deps / blocked-target
	// applied: fresh IsBlocked nil, cached IsBlocked set, deps unchanged, no target flip
	mkPreserve := func(name string, setup func(st *storeState, in *snapshotInputs)) {
		st, in := base()
		setup(&st, &in)
		out = append(out, mergeFixture{"marg_preserve_" + name, st, in})
	}
	tb := true
	mkPreserve("inapplicable", func(st *storeState, in *snapshotInputs) {
		st.beads["a"] = beadWith("a", "open", func(b *Bead) { b.IsBlocked = &tb })
		st.deps["a"] = []Dep{dep("a", "x")}
		fresh := beadWith("a", "open", func(b *Bead) { v := false; b.IsBlocked = &v })
		in.freshByID["a"] = fresh
		in.depMap["a"] = []Dep{dep("a", "x")}
	})
	mkPreserve("no-cached", func(st *storeState, in *snapshotInputs) {
		st.beads["a"] = bead("a", "open") // cached IsBlocked nil
		st.deps["a"] = []Dep{dep("a", "x")}
		in.freshByID["a"] = bead("a", "open") // fresh IsBlocked nil
		in.depMap["a"] = []Dep{dep("a", "x")}
	})
	mkPreserve("applied", func(st *storeState, in *snapshotInputs) {
		st.beads["a"] = beadWith("a", "open", func(b *Bead) { b.IsBlocked = &tb })
		st.deps["a"] = []Dep{dep("a", "x")}
		in.freshByID["a"] = bead("a", "open") // fresh IsBlocked nil
		in.depMap["a"] = []Dep{dep("a", "x")}
	})
	mkPreserve("blocked-deps", func(st *storeState, in *snapshotInputs) {
		st.beads["a"] = beadWith("a", "open", func(b *Bead) { b.IsBlocked = &tb })
		st.deps["a"] = []Dep{dep("a", "x")}
		in.freshByID["a"] = bead("a", "open")
		in.depMap["a"] = []Dep{dep("a", "different")} // deps changed ⇒ preserve blocked
	})
	mkPreserve("blocked-target", func(st *storeState, in *snapshotInputs) {
		st.beads["a"] = beadWith("a", "open", func(b *Bead) { b.IsBlocked = &tb })
		st.deps["a"] = []Dep{{IssueID: "a", DependsOnID: "t", Type: "blocks"}}
		st.beads["t"] = bead("t", "open")
		in.freshByID["a"] = bead("a", "open")
		in.freshByID["t"] = bead("t", "closed") // target status flipped ⇒ preserve blocked
		in.depMap["a"] = []Dep{{IssueID: "a", DependsOnID: "t", Type: "blocks"}}
		in.depMap["t"] = nil
	})
	return out
}

// ---------------------------------------------------------------------------
// Tier 2: seeded pseudo-random multi-row states
// ---------------------------------------------------------------------------

func genSeededStates(seed int64, count int) []mergeFixture {
	rng := rand.New(rand.NewSource(seed))
	out := make([]mergeFixture, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, genRandomState(rng, i))
	}
	return out
}

func genRandomState(rng *rand.Rand, idx int) mergeFixture {
	const startSeq = uint64(100)
	mutated := rng.Intn(2) == 0
	st := storeState{
		beads: map[string]Bead{}, deps: map[string][]Dep{}, dirty: map[string]struct{}{},
		beadSeq: map[string]uint64{}, localBeadAt: map[string]time.Time{}, deletedSeq: map[string]uint64{},
		backingIsBd: rng.Intn(2) == 0,
	}
	in := snapshotInputs{
		freshByID: map[string]Bead{}, confirmedClosed: map[string]Bead{}, depMap: map[string][]Dep{},
		useFreshDeps: rng.Intn(2) == 0, startSeq: startSeq, now: fxNow,
	}
	if mutated {
		st.mutationSeq = startSeq + uint64(1+rng.Intn(100))
	} else {
		st.mutationSeq = startSeq
	}

	nRows := 1 + rng.Intn(6)
	statuses := []string{"open", "in_progress", "closed"}
	recencies := []string{"none", "recent", "boundary", "justover", "stale", "now"}
	for r := 0; r < nRows; r++ {
		id := fmt.Sprintf("r%d", r)
		presence := rng.Intn(4) // 0 both,1 snap,2 cache,3 neither
		cachedPresent := presence == 0 || presence == 2
		freshPresent := presence == 0 || presence == 1

		if cachedPresent {
			cb := bead(id, statuses[rng.Intn(3)])
			applyRandomFields(rng, &cb)
			st.beads[id] = cb
			if rng.Intn(3) != 0 {
				st.deps[id] = randDeps(rng, id)
			}
		}
		if freshPresent {
			fb := bead(id, statuses[rng.Intn(3)])
			applyRandomFields(rng, &fb)
			in.freshByID[id] = fb
			if in.useFreshDeps && rng.Intn(3) != 0 {
				in.depMap[id] = randDeps(rng, id)
			}
			if cachedPresent && !isClosed(st.beads[id]) && rng.Intn(4) == 0 {
				in.confirmedClosed[id] = beadWith(id, "closed", func(_ *Bead) {})
			}
		}
		// Fences (respect V: V4 seq <= mutationSeq; quiescent ⇒ <= startSeq
		// since mutationSeq==startSeq; tombstone ⇒ no live row).
		if rng.Intn(2) == 0 {
			st.beadSeq[id] = randFence(rng, startSeq, st.mutationSeq)
		}
		if !cachedPresent && rng.Intn(2) == 0 {
			st.deletedSeq[id] = randFence(rng, startSeq, st.mutationSeq)
		}
		if rng.Intn(2) == 0 {
			if t, ok := recencyValue2(recencies[rng.Intn(len(recencies))]); ok {
				st.localBeadAt[id] = t
			}
		}
		if rng.Intn(3) == 0 {
			st.dirty[id] = struct{}{}
		}
		// Orphan deps-only entries for a never-present id occasionally.
		if presence == 3 && rng.Intn(2) == 0 {
			st.deps[id] = randDeps(rng, id)
		}
	}
	return mergeFixture{name: fmt.Sprintf("seed%d_case%d", 0, idx), st: st, in: in}
}

func recencyValue2(cell string) (time.Time, bool) {
	if cell == "now" {
		return fxNow, true
	}
	return recencyValue(cell)
}

// randFence returns a fence value in [1, mutationSeq] (V4). When mutationSeq >
// startSeq (mutated regime) it biases toward the (startSeq, mutationSeq] band so
// the > startSeq fence arms are exercised; it never exceeds mutationSeq, so a
// quiescent state (mutationSeq==startSeq) automatically satisfies invariant Q.
func randFence(rng *rand.Rand, startSeq, mutationSeq uint64) uint64 {
	if mutationSeq > startSeq && rng.Intn(2) == 0 {
		return startSeq + 1 + uint64(rng.Intn(int(mutationSeq-startSeq)))
	}
	return uint64(1 + rng.Intn(int(startSeq)))
}

func applyRandomFields(rng *rand.Rand, b *Bead) {
	if rng.Intn(2) == 0 {
		b.Title = fmt.Sprintf("t%d", rng.Intn(3))
	}
	if rng.Intn(3) == 0 {
		b.Labels = []string{fmt.Sprintf("l%d", rng.Intn(2))}
	}
	if rng.Intn(3) == 0 {
		v := rng.Intn(2) == 0
		b.IsBlocked = &v
	}
	if rng.Intn(3) == 0 {
		b.Metadata = StringMap{"k": fmt.Sprintf("%d", rng.Intn(2))}
	}
	if rng.Intn(3) == 0 {
		b.Needs = []string{fmt.Sprintf("n%d", rng.Intn(2))}
	}
	if rng.Intn(3) == 0 {
		b.Dependencies = []Dep{dep(b.ID, fmt.Sprintf("d%d", rng.Intn(2)))}
	}
}

func randDeps(rng *rand.Rand, id string) []Dep {
	switch rng.Intn(4) {
	case 0:
		return nil
	case 1:
		return []Dep{}
	default:
		n := 1 + rng.Intn(2)
		ds := make([]Dep, n)
		for i := range ds {
			ds[i] = dep(id, fmt.Sprintf("t%d", rng.Intn(3)))
		}
		return ds
	}
}

func isClosed(b Bead) bool { return b.Status == "closed" }

// ---------------------------------------------------------------------------
// Differential tests over the tiers
// ---------------------------------------------------------------------------

func TestReconcileMergeDifferential_Grid(t *testing.T) {
	for _, f := range genGridStates() {
		f := f
		for _, bd := range []bool{false, true} {
			st := cloneStoreState(f.st)
			st.backingIsBd = bd
			assertDifferential(t, f.name+backingSuffix(bd), st, cloneSnapshotInputs(f.in))
		}
	}
}

func TestReconcileMergeDifferential_Marginal(t *testing.T) {
	for _, f := range genMarginalStates() {
		f := f
		for _, bd := range []bool{false, true} {
			st := cloneStoreState(f.st)
			st.backingIsBd = bd
			assertDifferential(t, f.name+backingSuffix(bd), st, cloneSnapshotInputs(f.in))
		}
	}
}

func TestReconcileMergeDifferential_Seeded(t *testing.T) {
	states := genSeededStates(1, 12000)
	for _, f := range states {
		assertDifferential(t, f.name, cloneStoreState(f.st), cloneSnapshotInputs(f.in))
	}
}

func backingSuffix(bd bool) string {
	if bd {
		return "/bd"
	}
	return "/mem"
}

// ---------------------------------------------------------------------------
// Coverage assertions
// ---------------------------------------------------------------------------

func classifyAllInto(rec *coverageRecorder, states []mergeFixture) {
	// Classify the SAME (deep-cloned) state+inputs the differential actually
	// runs — cloneStoreState/cloneSnapshotInputs collapse empty deps slices to
	// nil exactly as production's cloneDeps does, so classification and
	// execution can never disagree on an unreachable empty-deps cell.
	for _, f := range states {
		for _, bd := range []bool{false, true} {
			st := cloneStoreState(f.st)
			st.backingIsBd = bd
			in := cloneSnapshotInputs(f.in)
			for _, id := range rowIDUniverse(st, in) {
				rec.record(classifyRow(st, in, id))
			}
		}
	}
}

func TestReconcileMergeCoverage_AllCellsExecuted(t *testing.T) {
	rec := newCoverageRecorder()
	classifyAllInto(rec, genGridStates())
	classifyAllInto(rec, genMarginalStates())
	classifyAllInto(rec, mergeFixtures())
	classifyAllInto(rec, genSeededStates(1, 3000))

	// 1. Full guard-axis cross: every intended guard cell must be observed.
	var missing []string
	forEachGuardCell(func(spec guardCellSpec) {
		gc := guardCell{spec.regime, spec.presence, spec.tomb, spec.seq, spec.recency, spec.changed, expectedStatusPair(spec)}
		if rec.guards[gc] == 0 {
			missing = append(missing, fmt.Sprintf("%+v", gc))
		}
	})
	if len(missing) > 0 {
		t.Fatalf("%d guard cells never executed (generator/classifier drift):\n%s",
			len(missing), joinLimited(missing, 25))
	}

	// 2. Marginal coverage: every value of every soft/hardened axis observed.
	// Empty (non-nil) deps slices are V-excluded: production's cloneDeps and
	// depsFromBeadFields collapse them to nil, so absent / present-nil /
	// present-nonempty are the only reachable deps-presence cells.
	requireMarginal(t, rec, "dirty", []string{"true", "false"})
	requireMarginal(t, rec, "depMapCell", []string{"na", "nil", "nonempty"})
	requireMarginal(t, rec, "fieldDeps", []string{"na", "none", "needs", "deps", "both"})
	requireMarginal(t, rec, "cachedDeps", []string{"absent", "nil", "nonempty"})
	requireMarginal(t, rec, "useFreshDeps", []string{"true", "false"})
	requireMarginal(t, rec, "backingIsBd", []string{"true", "false"})
	requireMarginal(t, rec, "confirmedClosed", []string{"true", "false"})
	requireMarginal(t, rec, "preserveOutcome", []string{"inapplicable", "no-cached", "applied", "blocked-deps", "blocked-target", "na"})
	requireMarginal(t, rec, "recency", []string{"none", "recent", "boundary", "justover", "stale", "now"})
}

func TestReconcileMergeCoverage_QuiescentCellsExecuted(t *testing.T) {
	// The Branch-B deletion precondition: every B-reachable (quiescent) guard
	// cell must have been exercised against the frozen Branch B.
	rec := newCoverageRecorder()
	classifyAllInto(rec, genGridStates())
	classifyAllInto(rec, genMarginalStates())
	classifyAllInto(rec, mergeFixtures())
	classifyAllInto(rec, genSeededStates(1, 3000))

	var missing []string
	forEachGuardCell(func(spec guardCellSpec) {
		if spec.regime != "quiescent" {
			return
		}
		gc := guardCell{spec.regime, spec.presence, spec.tomb, spec.seq, spec.recency, spec.changed, expectedStatusPair(spec)}
		if rec.guards[gc] == 0 {
			missing = append(missing, fmt.Sprintf("%+v", gc))
		}
	})
	if len(missing) > 0 {
		t.Fatalf("%d quiescent guard cells never executed — Branch B deletion is NOT safe:\n%s",
			len(missing), joinLimited(missing, 25))
	}
}

func expectedStatusPair(spec guardCellSpec) string {
	switch spec.presence {
	case "both":
		return spec.cachedStatus + ">" + spec.freshStatus
	case "snap":
		return "?>" + spec.freshStatus
	case "cache":
		return spec.cachedStatus + ">?"
	default:
		return "na"
	}
}

func requireMarginal(t *testing.T, rec *coverageRecorder, axis string, vals []string) {
	t.Helper()
	for _, v := range vals {
		if rec.marginal[axis+"="+v] == 0 {
			t.Errorf("marginal coverage gap: %s=%s never observed", axis, v)
		}
	}
}

func joinLimited(ss []string, n int) string {
	if len(ss) > n {
		ss = append(ss[:n:n], fmt.Sprintf("... (+%d more)", len(ss)-n))
	}
	out := ""
	for _, s := range ss {
		out += "  " + s + "\n"
	}
	return out
}
