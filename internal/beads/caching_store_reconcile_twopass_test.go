package beads

import (
	"fmt"
	"testing"
	"time"
)

// Tier 4: metamorphic two-pass sequences. From a base state we run one merge,
// apply an intervening REAL-primitive operation (the lifecycle a single-pass
// grid cannot reach — Delete→Update, Update→Delete, event-absorb→update, etc.),
// then re-run the differential on the compound post-op state with a pass-2
// clock advanced by a delta-now drawn from the fence-window boundary set and a
// startSeq captured either before or after the op. This is the fence-lifecycle
// coverage (#2210/#2987 shape) and the delta-now expiry axis.

// extractStoreState snapshots a live store's merge-relevant state.
func extractStoreState(c *CachingStore) storeState {
	_, isBd := c.backing.(*BdStore)
	return storeState{
		beads:        cloneBeadMap(c.beads),
		deps:         cloneDepMap(c.deps),
		depsComplete: c.depsComplete,
		dirty:        cloneDirty(c.dirty),
		beadSeq:      cloneU64Map(c.beadSeq),
		localBeadAt:  cloneTimeMap(c.localBeadAt),
		deletedSeq:   cloneU64Map(c.deletedSeq),
		mutationSeq:  c.mutationSeq,
		backingIsBd:  isBd,
	}
}

// twoPassOp is one intervening operation using the real primitives.
type twoPassOp struct {
	name  string
	apply func(c *CachingStore, id string, opNow time.Time)
}

func twoPassOps() []twoPassOp {
	return []twoPassOp{
		{"tombstone", func(c *CachingStore, id string, _ time.Time) {
			c.tombstoneLocked(id, c.noteMutationLocked(id))
		}},
		{"markDirty", func(c *CachingStore, id string, _ time.Time) {
			c.markDirtyLocked(id)
		}},
		{"event_absorb_seqKeep", func(c *CachingStore, id string, opNow time.Time) {
			// ApplyEvent-shape: bump the seq fence then absorb keeping it.
			c.noteMutationLocked(id)
			c.absorbFreshLocked(id, beadWith(id, "in_progress", func(b *Bead) { b.Title = "evt" }), opNow, absorbOpts{
				depsMode: depsFromFields, seqMode: seqKeep, clearDirty: true,
			})
		}},
		{"local_update", func(c *CachingStore, id string, opNow time.Time) {
			// Update-shape with a CONTROLLED recency stamp (noteLocalMutationLocked
			// reads the real wall clock, which we cannot inject; replicate its
			// effect deterministically at opNow).
			c.mutationSeq++
			c.beadSeq[id] = c.mutationSeq
			c.localBeadAt[id] = opNow
			c.absorbFreshLocked(id, beadWith(id, "open", func(b *Bead) { b.Title = "upd" }), opNow, absorbOpts{
				depsMode: depsFromFields, seqMode: seqKeep, clearDirty: true,
			})
		}},
		{"tombstone_then_update", func(c *CachingStore, id string, opNow time.Time) {
			// D1' genesis: Delete then a post-tombstone Update attempt.
			c.tombstoneLocked(id, c.noteMutationLocked(id))
			c.mutationSeq++
			c.beadSeq[id] = c.mutationSeq
			c.localBeadAt[id] = opNow
			c.absorbFreshLocked(id, bead(id, "open"), opNow, absorbOpts{
				depsMode: depsFromFields, seqMode: seqKeep, clearDirty: true,
			})
		}},
	}
}

var deltaNows = []time.Duration{0, 2500 * time.Millisecond, 5 * time.Second, 5001 * time.Millisecond, time.Hour}

func TestReconcileMergeDifferential_TwoPass(t *testing.T) {
	bases := []mergeFixture{}
	// A handful of grid states plus a few seeded states as pass-1 inputs.
	grid := genGridStates()
	for i := 0; i < len(grid); i += 40 { // sample the grid to bound runtime
		bases = append(bases, grid[i])
	}
	bases = append(bases, genSeededStates(7, 60)...)

	ops := twoPassOps()
	targetIDs := []string{"a", "r0"}

	for _, base := range bases {
		for _, op := range ops {
			for _, dn := range deltaNows {
				for _, capAfter := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/dn=%s/after=%v", base.name, op.name, dn, capAfter)

					// Pass 1: differential on the base, then advance a live NEW
					// store through pass 1 + the intervening op.
					st0 := cloneStoreState(base.st)
					in1 := cloneSnapshotInputs(base.in)
					assertDifferential(t, name+"/pass1", cloneStoreState(st0), cloneSnapshotInputs(in1))

					live := cloneSnapshotInputs(in1) // pass1's preserve mutates freshByID
					c, _ := newMergeHarnessStore(st0)
					c.mu.Lock()
					c.mergeSnapshotLocked(live.freshByID, live.confirmedClosed, live.depMap, live.useFreshDeps, live.startSeq, live.now)
					seqBefore := c.mutationSeq
					opNow := in1.now
					id := targetIDs[0]
					if _, ok := c.beads["r0"]; ok {
						id = "r0"
					}
					op.apply(c, id, opNow)
					seqAfter := c.mutationSeq
					st2 := extractStoreState(c)
					c.mu.Unlock()

					// Pass 2: fresh differential on the compound post-op state.
					startSeq2 := seqAfter
					if !capAfter {
						startSeq2 = seqBefore // op raced the scan
					}
					in2 := snapshotInputs{
						freshByID:    cloneBeadMap(in1.freshByID),
						depMap:       cloneDepMap(in1.depMap),
						useFreshDeps: in1.useFreshDeps,
						startSeq:     startSeq2,
						now:          in1.now.Add(dn),
					}
					// V4: startSeq <= mutationSeq. seqBefore/seqAfter both satisfy it.
					assertDifferential(t, name+"/pass2", st2, in2)
				}
			}
		}
	}
}
