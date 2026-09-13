package beads

import (
	"testing"
	"time"
)

// Fixed reference clock for all fixtures; recency offsets are relative to it.
var fxNow = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

func fxRecent() time.Time   { return fxNow.Add(-2 * time.Second) }         // well inside window
func fxBoundary() time.Time { return fxNow.Add(-5 * time.Second) }         // exactly 5s → still recent
func fxJustOver() time.Time { return fxNow.Add(-5001 * time.Millisecond) } // 5.001s → stale
func fxStale() time.Time    { return fxNow.Add(-time.Hour) }               // far stale

func bead(id, status string) Bead {
	return Bead{ID: id, Title: id, Status: status, Type: "task", CreatedAt: fxNow}
}

func beadWith(id, status string, mut func(*Bead)) Bead {
	b := bead(id, status)
	mut(&b)
	return b
}

func dep(issue, dependsOn string) Dep {
	return Dep{IssueID: issue, DependsOnID: dependsOn, Type: "blocks"}
}

type mergeFixture struct {
	name string
	st   storeState
	in   snapshotInputs
}

// mergeFixtures enumerates the §1.4 cells (B1-B11), the §2 deltas, and the
// bug-lineage regression shapes. Both regimes are represented.
func mergeFixtures() []mergeFixture {
	var fx []mergeFixture

	// Regime scaffolding: quiescent has mutationSeq==startSeq and all fences
	// <= startSeq; mutated has mutationSeq>startSeq and may fence > startSeq.
	const qseq = uint64(100)
	const mseq = uint64(200)
	quiIn := func() snapshotInputs {
		return snapshotInputs{startSeq: qseq, now: fxNow, useFreshDeps: true}
	}
	mutIn := func() snapshotInputs {
		return snapshotInputs{startSeq: qseq, now: fxNow, useFreshDeps: true}
	}
	_ = mutIn

	// --- B1: quiescent, in snapshot ∧ cached ∧ not recent ∧ changed → absorb ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": beadWith("a", "open", func(b *Bead) { b.Title = "new" })}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "B1_absorb_changed",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "y")}},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B2/D5: quiescent absorb, recent, NOT beadChanged → NEW keeps fences ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": bead("a", "open")}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "B2_D5_recent_no_conflict",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B3/D4/D2: quiescent recency-keep, cached deps present → keep cached deps ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": beadWith("a", "closed", func(_ *Bead) {})} // status change ⇒ beadChanged
		in.depMap = map[string][]Dep{"a": {dep("a", "fresh")}}
		fx = append(fx, mergeFixture{
			name: "B3_D4_recency_keep_with_cached_deps",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "cached")}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B3/D2: quiescent recency-keep, NO cached deps entry → depsComplete flip ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": bead("a", "closed")}
		in.depMap = map[string][]Dep{"a": {dep("a", "fresh")}}
		fx = append(fx, mergeFixture{
			name: "B3_D2_recency_keep_no_cached_deps",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{}, // no entry for a
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B4: quiescent, in snapshot ∧ NOT cached → created ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": bead("a", "open")}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "B4_created",
			st: storeState{
				beads:       map[string]Bead{},
				deps:        map[string][]Dep{},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B4-orphan/D5: created over a stale orphan fence, recent → keep fences ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": bead("a", "open")}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "B4orphan_D5_recent_orphan_now_in_snapshot",
			st: storeState{
				beads:       map[string]Bead{},
				deps:        map[string][]Dep{},
				beadSeq:     map[string]uint64{"a": 88},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B5: quiescent, missing ∧ cached ∧ non-closed ∧ recent → carry (skip) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "B5_evict_recency_keep",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B6: quiescent, missing ∧ cached ∧ closed → evict, no notification ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "B6_evict_closed",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "closed")},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B7: quiescent, missing ∧ cached ∧ non-closed ∧ stale → evict + closed ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		in.confirmedClosed = map[string]Bead{"a": beadWith("a", "closed", func(b *Bead) { b.Title = "auth" })}
		fx = append(fx, mergeFixture{
			name: "B7_evict_confirmed_closed",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				localBeadAt: map[string]time.Time{"a": fxStale()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B8/D3': quiescent orphan fences, recent → keep ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "B8_D3prime_orphan_recent",
			st: storeState{
				beads:       map[string]Bead{},
				dirty:       map[string]struct{}{"a": {}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B8: quiescent orphan fences, stale → GC'd (≡ B) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "B8_orphan_stale_gc",
			st: storeState{
				beads:       map[string]Bead{},
				dirty:       map[string]struct{}{"a": {}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxStale()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B9/D1': quiescent tombstone with recent localAt → keep-all ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "B9_D1prime_tombstone_recent",
			st: storeState{
				beads:       map[string]Bead{},
				deletedSeq:  map[string]uint64{"a": 95},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- B9: quiescent tombstone, stale → GC'd (≡ B wholesale wipe) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "B9_tombstone_stale_gc",
			st: storeState{
				beads:       map[string]Bead{},
				deletedSeq:  map[string]uint64{"a": 95},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- Orphan deps-only, recent → kept (D3' deps family, council delete-B gap) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "orphan_deps_only_recent_kept",
			st: storeState{
				beads:       map[string]Bead{},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- Orphan deps-only, stale → GC'd (immortal-deps regression guard) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{}
		fx = append(fx, mergeFixture{
			name: "orphan_deps_only_stale_gc",
			st: storeState{
				beads:       map[string]Bead{},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- MUTATED / D1: tombstone unprotected orphan the sweep collects (vs A) ---
	{
		in := snapshotInputs{startSeq: qseq, now: fxNow, useFreshDeps: true}
		in.freshByID = map[string]Bead{"a": bead("a", "open")}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "D1_mutated_tombstone_orphan_gc",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				deletedSeq:  map[string]uint64{"gone": 95}, // orphan tombstone, seq<=startSeq
				mutationSeq: mseq,                          // mutated regime
			},
			in: in,
		})
	}

	// --- MUTATED / D3: orphan dirty/beadSeq leaked, sweep collects (vs A) ---
	{
		in := snapshotInputs{startSeq: qseq, now: fxNow, useFreshDeps: true}
		in.freshByID = map[string]Bead{"a": bead("a", "open")}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "D3_mutated_orphan_fences_gc",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				dirty:       map[string]struct{}{"gone": {}},
				beadSeq:     map[string]uint64{"gone": 80},
				mutationSeq: mseq,
			},
			in: in,
		})
	}

	// --- MUTATED / fence arm: beadSeq > startSeq keeps the row (skipFenced) ---
	{
		in := snapshotInputs{startSeq: qseq, now: fxNow, useFreshDeps: true}
		in.freshByID = map[string]Bead{"a": beadWith("a", "open", func(b *Bead) { b.Title = "stale-scan" })}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "mutated_beadSeq_fence_skip",
			st: storeState{
				beads:       map[string]Bead{"a": beadWith("a", "open", func(b *Bead) { b.Title = "local-write" })},
				deps:        map[string][]Dep{"a": {dep("a", "x")}},
				beadSeq:     map[string]uint64{"a": 150}, // > startSeq
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: mseq,
			},
			in: in,
		})
	}

	// --- MUTATED / tombstone fence: deletedSeq > startSeq keeps eviction skip ---
	{
		in := snapshotInputs{startSeq: qseq, now: fxNow, useFreshDeps: true}
		in.freshByID = map[string]Bead{"a": bead("a", "open")}
		in.depMap = map[string][]Dep{"a": {dep("a", "x")}}
		fx = append(fx, mergeFixture{
			name: "mutated_tombstone_fence_absorb_skip",
			st: storeState{
				beads:       map[string]Bead{},
				deletedSeq:  map[string]uint64{"a": 150}, // delete raced the scan
				mutationSeq: mseq,
			},
			in: in,
		})
	}

	// --- #2210 shape: local DepAdd inside window, snapshot lags (recency keep) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": beadWith("a", "in_progress", func(_ *Bead) {})}
		in.depMap = map[string][]Dep{"a": {}} // snapshot dropped the just-added dep
		fx = append(fx, mergeFixture{
			name: "reg_2210_local_depadd_in_window",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "just-added")}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxRecent()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- nil-vs-empty deps classification: must NOT emit bead.updated ---
	{
		in := quiIn()
		in.useFreshDeps = true
		in.freshByID = map[string]Bead{"a": bead("a", "open")}
		in.depMap = map[string][]Dep{} // fresh deps nil
		fx = append(fx, mergeFixture{
			name: "reg_nil_vs_empty_deps",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {}}, // cached empty (non-nil)
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- boundary recency 5.000s: still recent (keeps) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": beadWith("a", "closed", func(_ *Bead) {})}
		in.depMap = map[string][]Dep{"a": {dep("a", "fresh")}}
		fx = append(fx, mergeFixture{
			name: "boundary_recency_5000ms_recent",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "cached")}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxBoundary()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	// --- boundary recency 5.001s: stale (absorbs) ---
	{
		in := quiIn()
		in.freshByID = map[string]Bead{"a": beadWith("a", "closed", func(_ *Bead) {})}
		in.depMap = map[string][]Dep{"a": {dep("a", "fresh")}}
		fx = append(fx, mergeFixture{
			name: "boundary_recency_5001ms_stale",
			st: storeState{
				beads:       map[string]Bead{"a": bead("a", "open")},
				deps:        map[string][]Dep{"a": {dep("a", "cached")}},
				beadSeq:     map[string]uint64{"a": 90},
				localBeadAt: map[string]time.Time{"a": fxJustOver()},
				mutationSeq: qseq,
			},
			in: in,
		})
	}

	return fx
}

func TestReconcileMergeDifferential_Fixtures(t *testing.T) {
	for _, f := range mergeFixtures() {
		f := f
		t.Run(f.name, func(t *testing.T) {
			// Run both backing variants so the depsForReconcileLocked fallback
			// is exercised on the same shapes.
			for _, bd := range []bool{false, true} {
				st := cloneStoreState(f.st)
				st.backingIsBd = bd
				variant := "memBacking"
				if bd {
					variant = "bdBacking"
				}
				assertDifferential(t, f.name+"/"+variant, st, cloneSnapshotInputs(f.in))
			}
		})
	}
}
