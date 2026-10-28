package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
)

// mcShapedRigFixture seeds store with the shape of maintainer-city's gascity
// rig the review measured: 26 closed v1 poured roots holding 27 children, 2
// open stepless junk roots, a bd proto template, and a closed root whose
// closure holds a reopened step.
func mcShapedRigFixture(t *testing.T, store beads.Store) {
	t.Helper()
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	closeID := func(id string) {
		t.Helper()
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 26; i++ {
		root := mk(beads.Bead{Title: fmt.Sprintf("mol-poured-%02d", i), Type: "molecule"})
		children := 1
		if i == 0 {
			children = 2
		}
		for c := 0; c < children; c++ {
			closeID(mk(beads.Bead{Title: "step", Type: "step", ParentID: root.ID}).ID)
		}
		closeID(root.ID)
	}
	for i := 0; i < 2; i++ {
		mk(beads.Bead{Title: fmt.Sprintf("junk-%d", i), Type: "molecule"})
	}
	closeID(mk(beads.Bead{Title: "mol-proto", Type: "molecule", Labels: []string{bdTemplateLabel}}).ID)
	live := mk(beads.Bead{Title: "mol-live-step", Type: "molecule"})
	liveStep := mk(beads.Bead{Title: "reopened step", Type: "step", ParentID: live.ID})
	closeID(liveStep.ID)
	closeID(live.ID)
	inProgress := "in_progress"
	if err := store.Update(liveStep.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
}

// bdLegs builds n MC-shaped rig legs, each a real BdStore (driven by a fake
// bd that counts its invocations — each one a subprocess in production)
// behind the production CachingStore, and the split env's WorkLegs over them.
func bdLegs(t *testing.T, n int) (splitEnv, WorkLegs, *atomic.Int64) {
	t.Helper()
	e := newSplitEnvWithRig(t, true)
	var calls atomic.Int64
	rigs := map[string]beads.Store{}
	e.cfg.Rigs = nil
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("rig-%c", 'A'+i)
		e.cfg.Rigs = append(e.cfg.Rigs, config.Rig{Name: name, Path: filepath.Join(e.cityPath, "rigs", name)})
		fb := beadstest.NewFakeBd()
		bd := beads.NewBdStore("/fake-bd", func(dir, name string, args ...string) ([]byte, error) {
			calls.Add(1)
			return fb.Run(dir, name, args...)
		})
		mcShapedRigFixture(t, bd)
		cache := beads.NewCachingStore(bd, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		rigs[name] = cache
	}
	legs := workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), rigs)
	return e, legs, &calls
}

// Must-fix: count the bd invocations (subprocesses on maintainer-city, ~0.75
// s each) one work-arm pass makes, with 1 and 7 MC-shaped legs behind the
// production CachingStore. A dry pass only lists; an enforced pass spends one
// budget shared across every leg.
func TestWispGCWorkArmBackingCallsPerPass(t *testing.T) {
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut) })
	for _, n := range []int{1, 7} {
		t.Run(fmt.Sprintf("%d-legs", n), func(t *testing.T) {
			e, legs, calls := bdLegs(t, n)
			gc := newWorkMoleculeGC()
			pass := func(now time.Time) int64 {
				calls.Store(0)
				withCloseAbandonedEnforced(t, func() {
					withCloseAbandonedTTL(t, 5*time.Minute, func() {
						if _, err := gc.pass(context.Background(), legs, e.graphStore(), time.Hour, now); err != nil {
							t.Logf("pass: %v", err)
						}
					})
				})
				return calls.Load()
			}
			now := time.Now().Add(48 * time.Hour)
			dry := []int64{pass(now), pass(now)}
			var enforced []int64
			withWorkMoleculesEnforced(t, func() {
				for i := 0; i < 8; i++ {
					enforced = append(enforced, pass(now))
				}
			})
			t.Logf("%d leg(s): dry-run bd calls per pass %v; enforced %v (budget %d walks / %s per pass)", n, dry, enforced, wispGCWorkArmWalkBudget, wispGCWorkArmWallBudget)
			for _, c := range dry {
				if c > int64(4*n) {
					t.Errorf("a dry-run pass made %d bd calls over %d leg(s); it must only list", c, n)
				}
			}
			// One budget per pass across all legs, and batched reads. A pass
			// lists the legs it visits (3 calls each) and walks at most 5
			// closures of a root and one child. Each walk costs 8 calls: the
			// gc.root_bead_id member list (2), one batched children read per
			// level (2 + 1), the live strand guard on the root (2) and the
			// delete (1); the roots' live rows come from one prefetch. The
			// ceilings below hold that, so disabling the prefetch (+1 per walk)
			// or the batched level walk (per-node reads) fails them.
			ceiling := map[int]int64{1: 46, 7: 58}[n]
			for i, c := range enforced[1:] {
				if c > ceiling {
					t.Errorf("enforced pass %d made %d bd calls over %d leg(s), want at most %d", i+2, c, n, ceiling)
				}
			}
		})
	}
}

// Must-fix: the graph arm runs on the controller tick, and its holder guard
// asks every work leg whether a bead still holds a stepless root it cannot
// rule out from the root's own record. Count the bd invocations it makes over
// consecutive ticks, with 10 such roots and 5 bd-served legs: the legs answer
// once per root, and later ticks reuse that answer until the root changes or
// the recheck window passes. Both shapes that kept a root in front of the
// guard every tick: a dry run's would-close roots, which nothing closes, and
// roots a live source on the last leg holds open.
func TestWispGCGraphArmHolderGuardBackingCallsPerTick(t *testing.T) {
	prevOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prevOut) })
	const rootCount, legCount = 10, 5
	for _, enforce := range []bool{false, true} {
		t.Run(fmt.Sprintf("enforce=%v", enforce), func(t *testing.T) {
			e, legs, calls := bdLegs(t, legCount)
			graph := e.graphStore()
			lastRig := fmt.Sprintf("rig:rig-%c", 'A'+legCount-1)
			var lastLeg beads.Store
			legs.eachLeg(func(ref string, s beads.Store) {
				if ref == lastRig {
					lastLeg = s
				}
			})
			if lastLeg == nil {
				t.Fatalf("the legs have no %s", lastRig)
			}
			var roots, sources []beads.Bead
			for i := 0; i < rootCount; i++ {
				// A pre-pointer attached root-only wisp: molecule-typed, no
				// recorded source, so only the holder query can clear it.
				root, err := graph.Create(beads.Bead{Title: fmt.Sprintf("legacy attached %d", i), Type: "molecule", Metadata: map[string]string{
					beadmeta.KindMetadataKey: beadmeta.KindWisp,
				}})
				if err != nil {
					t.Fatal(err)
				}
				roots = append(roots, root)
				if enforce {
					src, err := lastLeg.Create(beads.Bead{Title: "live source", Type: "task", Metadata: map[string]string{
						beadmeta.MoleculeIDMetadataKey: root.ID,
					}})
					if err != nil {
						t.Fatal(err)
					}
					sources = append(sources, src)
				}
			}
			prevEnforced := closeAbandonedEnforced
			closeAbandonedEnforced = func() bool { return enforce }
			t.Cleanup(func() { closeAbandonedEnforced = prevEnforced })
			gc := newWispGC(5*time.Minute, time.Hour, 0)
			tick := func(now time.Time) int64 {
				t.Helper()
				calls.Store(0)
				withCloseAbandonedTTL(t, 5*time.Minute, func() {
					if _, err := gc.runGC(legs, beads.GraphStore{Store: graph}, beads.SessionStore{}, beads.MailStore{}, now); err != nil {
						t.Logf("runGC: %v", err)
					}
				})
				return calls.Load()
			}
			now := time.Now().Add(48 * time.Hour)
			first := tick(now)
			steady := []int64{tick(now.Add(10 * time.Minute)), tick(now.Add(20 * time.Minute))}
			if err := graph.SetMetadata(roots[0].ID, "probe", "touched"); err != nil {
				t.Fatal(err)
			}
			touched := tick(now.Add(30 * time.Minute))
			if enforce {
				if err := lastLeg.Close(sources[1].ID); err != nil {
					t.Fatal(err)
				}
			}
			rechecked := tick(now.Add(30*time.Minute + wispGCRefusalRecheck + time.Minute))
			t.Logf("%d root(s) x %d bd leg(s): bd calls per tick: first %d, steady %v, one root touched %d, after the recheck window %d", rootCount, legCount, first, steady, touched, rechecked)

			if first == 0 {
				t.Fatal("the first tick made no bd call: the fixture never put a root in front of the holder guard")
			}
			for i, c := range steady {
				if c != 0 {
					t.Errorf("tick %d made %d bd calls for roots whose holders the legs already answered, want 0", i+2, c)
				}
			}
			if perRoot := first / rootCount; touched == 0 || touched > perRoot {
				t.Errorf("a tick after one root changed made %d bd calls, want that root's own holder query (1..%d)", touched, perRoot)
			}
			if rechecked == 0 {
				t.Error("no tick asked the legs again after the recheck window")
			}
			for i, root := range roots {
				got, err := graph.Get(root.ID)
				if enforce && i == 1 {
					// Its source closed: the recheck closes it, and the same
					// pass purges it once it is past retention.
					if err == nil && got.Status != "closed" {
						t.Errorf("root %d (%s) status=%q after its source closed and the recheck, want closed or purged", i, root.ID, got.Status)
					}
					if err != nil && !errors.Is(err, beads.ErrNotFound) {
						t.Fatal(err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.Status != "open" {
					t.Errorf("root %d (%s) status=%q after the recheck, want open", i, root.ID, got.Status)
				}
			}
		})
	}
}
