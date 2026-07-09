package beads

import (
	"testing"
	"time"
)

// T4: exhaustive test of the pure reconcileMergeDecision over its input
// lattice, plus the §1.2 structural invariants.
//
// Scope of the oracle: expectedDecision is a hand-transcription of the same
// decision table the production switch encodes, so this test is DRIFT and
// STRUCTURAL-INVARIANT coverage — it catches an accidental future edit that
// moves one function out of step with the other, and the invariants pin
// type-level properties the switch must uphold on every lattice point. It is
// deliberately NOT an independent semantic oracle: a spec misunderstanding
// baked into both functions would pass here. The independent semantic ground
// truth is the frozen Branch A / Branch B differential gate
// (caching_store_reconcile_differential_test.go), which runs the real
// pre-collapse bodies and would fail on a wrong decision; this table sits on
// top of that as cheap, exhaustive drift protection.

func expectedDecision(in mergeRowInput) mergeDecision {
	switch {
	case in.freshExists:
		if in.deletedAtSeq > in.startSeq || in.beadAtSeq > in.startSeq {
			return mergeDecision{action: mergeSkipFenced, degradeDepsComplete: in.cachedExists && !in.hasCachedDeps}
		}
		if in.cachedExists && recentLocalMutation(in.localAt, in.now) && beadChanged(in.cached, in.fresh, in.skipLabels) {
			return mergeDecision{action: mergeSkipRecentLocal, degradeDepsComplete: !in.hasCachedDeps || depsChanged(in.cachedDeps, in.freshDeps)}
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
	case in.cachedExists:
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
	default:
		if in.deletedAtSeq > in.startSeq || in.beadAtSeq > in.startSeq {
			return mergeDecision{action: mergeSkipFenced}
		}
		if recentLocalMutation(in.localAt, in.now) {
			return mergeDecision{action: mergeSkipRecentLocal}
		}
		return mergeDecision{action: mergeGCFences}
	}
}

func TestReconcileMergeDecision_Exhaustive(t *testing.T) {
	const startSeq = uint64(100)
	now := fxNow
	seqVals := []uint64{0, 99, 100, 101}
	recVals := []time.Time{{}, fxRecent(), fxBoundary(), fxJustOver(), fxStale()}
	// Beads chosen to drive beadChanged both ways under each status.
	beadOpen := bead("x", "open")
	beadOpenChanged := beadWith("x", "open", func(b *Bead) { b.Title = "changed" })
	beadClosed := bead("x", "closed")
	beadInProg := bead("x", "in_progress")
	beadSet := []Bead{beadOpen, beadOpenChanged, beadClosed, beadInProg}
	depSet := [][]Dep{nil, {dep("x", "d1")}}

	var count int
	for _, fe := range []bool{true, false} {
		for _, ce := range []bool{true, false} {
			for _, fresh := range beadSet {
				for _, cached := range beadSet {
					for _, fdeps := range depSet {
						for _, cdeps := range depSet {
							for _, hcd := range []bool{true, false} {
								for _, del := range seqVals {
									for _, bs := range seqVals {
										for _, rec := range recVals {
											for _, skip := range []bool{true, false} {
												in := mergeRowInput{
													freshExists:   fe,
													fresh:         fresh,
													freshDeps:     fdeps,
													cachedExists:  ce,
													cached:        cached,
													cachedDeps:    cdeps,
													hasCachedDeps: hcd,
													deletedAtSeq:  del,
													beadAtSeq:     bs,
													startSeq:      startSeq,
													localAt:       rec,
													now:           now,
													skipLabels:    skip,
												}
												got := reconcileMergeDecision(in)
												want := expectedDecision(in)
												if got != want {
													t.Fatalf("decision mismatch\n in=%+v\n got=%+v\n want=%+v", in, got, want)
												}
												assertDecisionInvariants(t, in, got)
												count++
											}
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if count < 10000 {
		t.Fatalf("lattice too small: %d points", count)
	}
}

func assertDecisionInvariants(t *testing.T, in mergeRowInput, d mergeDecision) {
	t.Helper()
	// INV-A: an uncached absorb-cell row can never yield mergeSkipRecentLocal.
	if in.freshExists && !in.cachedExists && d.action == mergeSkipRecentLocal {
		t.Fatalf("uncached absorb cell yielded mergeSkipRecentLocal: %+v", in)
	}
	// INV-B: mergeGCFences only when both rows absent.
	if d.action == mergeGCFences && (in.freshExists || in.cachedExists) {
		t.Fatalf("mergeGCFences with a present row: %+v", in)
	}
	// INV-C: degradeDepsComplete is only ever set on absorb-cell skip arms.
	if d.degradeDepsComplete {
		absorbCellSkip := in.freshExists &&
			(d.action == mergeSkipFenced || d.action == mergeSkipRecentLocal)
		if !absorbCellSkip {
			t.Fatalf("degradeDepsComplete set outside an absorb-cell skip arm: in=%+v d=%+v", in, d)
		}
	}
	// INV-D: eviction-cell never degrades depsComplete.
	if !in.freshExists && in.cachedExists && d.degradeDepsComplete {
		t.Fatalf("eviction cell degraded depsComplete: %+v", in)
	}
	// INV-E: notifications only accompany their action.
	switch d.action {
	case mergeAbsorb:
		if d.notification != "" && d.notification != "bead.created" && d.notification != "bead.updated" {
			t.Fatalf("absorb produced notification %q", d.notification)
		}
	case mergeEvict:
		if d.notification != "" && d.notification != "bead.closed" {
			t.Fatalf("evict produced notification %q", d.notification)
		}
	default:
		if d.notification != "" {
			t.Fatalf("action %v produced notification %q", d.action, d.notification)
		}
	}
}
