package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
)

// Must-fix 1: a leg full of busy idle molecules pins neither the lane nor its
// own purge. The leg cursor advances every pass and the arms alternate, so the
// expired closures on both legs are purged within a day of 10-minute passes,
// whatever the budget.
func TestWorkArmLegsAreNotStarved(t *testing.T) {
	for _, budget := range []int{1, 5} {
		for _, n := range []int{20, 40, 80} {
			t.Run(fmt.Sprintf("budget=%d/busy=%d", budget, n), func(t *testing.T) {
				legA, legB := newBatchDeleteWorkStore(), newBatchDeleteWorkStore()
				for i := 0; i < n; i++ {
					busy := stepless(t, legA, beads.Bead{Title: fmt.Sprintf("busy-%03d", i), Type: "molecule"})
					stepless(t, legA, beads.Bead{Title: "live step", Type: "step", ParentID: busy.ID})
				}
				expiredB := closedPoured(t, legB, "expired")
				expiredA := closedPoured(t, legA, "expiredA")
				e, legs := laneLegs(t, legA, legB)
				gc := newWorkMoleculeGC()
				base := time.Now().Add(2 * time.Hour)
				passes := 0
				withWorkArmBudget(t, budget, func() {
					enforcedLane(t, func() {
						for passes = 1; passes <= 6*24; passes++ {
							runLane(t, gc, legs, e.graphStore(), base.Add(time.Duration(passes)*10*time.Minute))
							if laneGone(legB, expiredB.ID) && laneGone(legA, expiredA.ID) {
								break
							}
						}
					})
				})
				if !laneGone(legB, expiredB.ID) || !laneGone(legA, expiredA.ID) {
					t.Fatalf("%d busy roots on leg A starved the purge for a day (leg B purged=%v, leg A purged=%v)", n, laneGone(legB, expiredB.ID), laneGone(legA, expiredA.ID))
				}
				t.Logf("both expired closures purged after %d passes", passes)
			})
		}
	}
}

// Must-fix 2: rotateAfter resumes after its cursor and wraps around.
func TestRotateAfterResumesAfterTheCursor(t *testing.T) {
	roots := []beads.Bead{{ID: "gc-3"}, {ID: "gc-1"}, {ID: "gc-4"}, {ID: "gc-2"}}
	ids := func(bs []beads.Bead) string {
		var out []string
		for _, b := range bs {
			out = append(out, b.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(rotateAfter(roots, "")); got != "gc-1,gc-2,gc-3,gc-4" {
		t.Errorf("rotateAfter(no cursor) = %s", got)
	}
	if got := ids(rotateAfter(roots, "gc-2")); got != "gc-3,gc-4,gc-1,gc-2" {
		t.Errorf("rotateAfter(gc-2) = %s, want gc-3,gc-4,gc-1,gc-2", got)
	}
	if got := ids(rotateAfter(roots, "gc-9")); got != "gc-1,gc-2,gc-3,gc-4" {
		t.Errorf("rotateAfter(past the end) = %s", got)
	}
}

// Must-fix 2: the leg cursor moves every pass, so with a budget of one walk a
// leg with more work than one pass still lets the next leg in. The fixture has
// three legs (the city work store, A, B): within three passes every leg comes
// first once, where a frozen cursor would hand A the walk every time.
func TestWorkArmLegCursorAdvancesEveryPass(t *testing.T) {
	legA, legB := newBatchDeleteWorkStore(), newBatchDeleteWorkStore()
	for i := 0; i < 3; i++ {
		closedPoured(t, legA, fmt.Sprintf("a-%d", i))
	}
	b := closedPoured(t, legB, "b")
	e, legs := laneLegs(t, legA, legB)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	withWorkArmBudget(t, 1, func() {
		enforcedLane(t, func() {
			for i := 0; i < 3; i++ {
				runLane(t, gc, legs, e.graphStore(), now)
			}
		})
	})
	if !laneGone(legB, b.ID) {
		t.Fatal("after three passes of one walk each over three legs, leg B was never reached: the leg cursor did not move")
	}
}

// Must-fix 2: within a leg, the root cursor resumes where the last visit
// stopped: with one walk per pass and three expired closures on the only leg,
// three passes purge all three (a cursor that restarted at the first root
// would still purge them, so the rows are forced to be re-listed in a fixed
// order and the first root is made un-purgeable).
func TestWorkArmRootCursorResumesWithinALeg(t *testing.T) {
	var asked []string
	inner := newBatchDeleteWorkStore()
	store := laneFailingDeleteStore{batchDeleteWorkStore: inner, asked: &asked}
	for i := 0; i < 3; i++ {
		closedPoured(t, inner, fmt.Sprintf("r-%d", i))
	}
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	withWorkArmBudget(t, 1, func() {
		enforcedLane(t, func() {
			for i := 0; i < 3; i++ {
				// Forget the failure memo, so only the cursor can move on.
				gc.notes.refused = nil
				runLane(t, gc, legs, e.graphStore(), now)
			}
		})
	})
	roots := map[string]bool{}
	for _, id := range asked {
		if b, err := inner.Get(id); err == nil && b.Type == "molecule" {
			roots[id] = true
		}
	}
	if len(roots) != 3 {
		t.Fatalf("three passes attempted %d distinct roots (%v), want 3: the root cursor did not resume", len(roots), asked)
	}
}

// laneRuntime is a split-city CityRuntime whose lane pass reads stores from
// the fixture.
func laneRuntime(t *testing.T, split bool) (*CityRuntime, splitEnv) {
	t.Helper()
	e := newSplitEnvWithRig(t, split)
	cfg := *e.cfg
	cfg.Daemon.WispGCInterval = "10m"
	cfg.Daemon.WispTTL = "1h"
	e.cfg = &cfg
	return &CityRuntime{
		cityPath:            e.cityPath,
		cityName:            e.cfg.Workspace.Name,
		cfg:                 e.cfg,
		rec:                 events.Discard,
		storageRoutes:       e.routes,
		standaloneCityStore: e.work,
		standaloneRigStores: map[string]beads.Store{e.rigName: newBatchDeleteWorkStore()},
		stdout:              &bytes.Buffer{},
		stderr:              &bytes.Buffer{},
		wg:                  newWispGC(time.Nanosecond, time.Hour, 0),
	}, e
}

// Should-fix 6: the lane's wiring. The tick wakes it; a single-store city and
// a quiescent one get no pass; a split city does.
func TestWorkGCLaneWiring(t *testing.T) {
	t.Run("tick-wakes-the-lane", func(t *testing.T) {
		cr, _ := laneRuntime(t, true)
		cr.tickWispGC(&tickPass{})
		select {
		case <-cr.workGCLaneOf().wakeCh:
		default:
			t.Fatal("the wisp GC tick did not wake the work lane")
		}
	})
	run := func(t *testing.T, cr *CityRuntime, e splitEnv) (purged bool) {
		rig := cr.standaloneRigStores[e.rigName]
		root := closedPoured(t, rig, "expired")
		enforcedLane(t, func() { cr.runWorkGCLanePass(context.Background(), time.Now().Add(3*time.Hour)) })
		return laneGone(rig, root.ID)
	}
	t.Run("split-city-runs", func(t *testing.T) {
		cr, e := laneRuntime(t, true)
		if !run(t, cr, e) {
			t.Fatalf("the lane did not purge an expired closure on a split city; stderr: %s", cr.stderr.(*bytes.Buffer).String())
		}
	})
	t.Run("single-store-city-skipped", func(t *testing.T) {
		cr, e := laneRuntime(t, false)
		if run(t, cr, e) {
			t.Fatal("the lane ran on a single-store city, where the graph arm owns these roots")
		}
		if _, ran := cr.workGCLaneOf().lastPassAge(time.Now()); ran {
			t.Fatal("the lane recorded a pass on a single-store city")
		}
	})
	t.Run("quiescent-city-skipped", func(t *testing.T) {
		cr, e := laneRuntime(t, true)
		cr.beadsQuiescent.Store(true)
		if run(t, cr, e) {
			t.Fatal("the lane read stores while the city was quiescent")
		}
	})
}

// wispLaneListCounter counts List calls.
type wispLaneListCounter struct {
	batchDeleteWorkStore
	mu    *sync.Mutex
	lists *int
}

func (s wispLaneListCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	*s.lists++
	s.mu.Unlock()
	return s.batchDeleteWorkStore.List(q)
}

// Should-fix 3/4: the pass stops starting work at the wall-clock deadline and
// as soon as its ctx is done: past either, it lists no leg and walks no root.
func TestWorkArmHonoursItsDeadlineAndCtx(t *testing.T) {
	mk := func(t *testing.T) (batchDeleteWorkStore, wispLaneListCounter, *int) {
		inner := newBatchDeleteWorkStore()
		for i := 0; i < 10; i++ {
			closedPoured(t, inner, fmt.Sprintf("r-%d", i))
		}
		lists := 0
		return inner, wispLaneListCounter{batchDeleteWorkStore: inner, mu: &sync.Mutex{}, lists: &lists}, &lists
	}
	count := func(inner batchDeleteWorkStore) int {
		rows, _ := inner.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, Type: "molecule"})
		return 10 - len(rows)
	}
	t.Run("deadline", func(t *testing.T) {
		inner, counting, lists := mk(t)
		e, legs := laneLegs(t, counting)
		prev := wispGCWorkArmWallBudget
		wispGCWorkArmWallBudget = time.Nanosecond
		t.Cleanup(func() { wispGCWorkArmWallBudget = prev })
		withWorkArmBudget(t, 100, func() {
			enforcedLane(t, func() { runLane(t, newWorkMoleculeGC(), legs, e.graphStore(), time.Now().Add(2*time.Hour)) })
		})
		if n := count(inner); n != 0 || *lists != 0 {
			t.Fatalf("a pass past its wall deadline listed %d time(s) and purged %d closure(s); it must start nothing", *lists, n)
		}
	})
	t.Run("ctx-done", func(t *testing.T) {
		inner, counting, lists := mk(t)
		e, legs := laneLegs(t, counting)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		enforcedLane(t, func() {
			withCloseAbandonedTTL(t, 5*time.Minute, func() {
				_, _ = newWorkMoleculeGC().pass(ctx, legs, e.graphStore(), time.Hour, time.Now().Add(2*time.Hour))
			})
		})
		if n := count(inner); n != 0 || *lists != 0 {
			t.Fatalf("a pass whose ctx was done listed %d time(s) and purged %d closure(s); it must start nothing", *lists, n)
		}
	})
}

// Should-fix 6: close-arm memos are pruned when their roots go; the graph
// arm's refusals too; and a failed listing keeps a leg's memos.
func TestWispGCMemoPruning(t *testing.T) {
	t.Run("close-memo", func(t *testing.T) {
		store := newBatchDeleteWorkStore()
		busy := stepless(t, store, beads.Bead{Title: "busy", Type: "molecule"})
		step := stepless(t, store, beads.Bead{Title: "live step", Type: "step", ParentID: busy.ID})
		e, legs := laneLegs(t, store)
		gc := newWorkMoleculeGC()
		now := time.Now().Add(30 * time.Minute)
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			if !hasMemo(gc.notes, workArmCloseMemo, busy.ID) {
				t.Fatal("busy root's not-closable outcome was not remembered")
			}
			for _, id := range []string{step.ID, busy.ID} {
				if err := store.Delete(id); err != nil {
					t.Fatal(err)
				}
			}
			runLane(t, gc, legs, e.graphStore(), now)
		})
		if hasMemo(gc.notes, workArmCloseMemo, busy.ID) {
			t.Fatal("a deleted root's close memo was kept")
		}
	})
	t.Run("graph-refusal", func(t *testing.T) {
		store := newGCStore([]beads.Bead{
			makeGCBead("mol-held", time.Now().Add(-3*time.Hour), "closed", "molecule"),
			{ID: "mol-held.step", Status: "in_progress", Type: "task", CreatedAt: time.Now().Add(-3 * time.Hour), ParentID: "mol-held"},
		})
		wg := newWispGC(5*time.Minute, time.Hour, 0).(*memoryWispGC)
		runOnce := func() {
			if _, err := wg.runGC(nil, beads.GraphStore{Store: store}, beads.SessionStore{}, beads.MailStore{Store: store}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		runOnce()
		if !hasMemo(wg.notes, "graph", "mol-held") {
			t.Fatal("the graph arm's refusal was not remembered")
		}
		for _, id := range []string{"mol-held.step", "mol-held"} {
			if err := store.Delete(id); err != nil {
				t.Fatal(err)
			}
		}
		runOnce()
		if hasMemo(wg.notes, "graph", "mol-held") {
			t.Fatal("a deleted root's graph refusal was kept")
		}
	})
	t.Run("failed-listing-keeps-memos", func(t *testing.T) {
		inner := newBatchDeleteWorkStore()
		busy := stepless(t, inner, beads.Bead{Title: "busy", Type: "molecule"})
		stepless(t, inner, beads.Bead{Title: "live step", Type: "step", ParentID: busy.ID})
		store := &toggleListStore{batchDeleteWorkStore: inner}
		e, legs := laneLegs(t, store)
		gc := newWorkMoleculeGC()
		now := time.Now().Add(30 * time.Minute)
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			store.fail = true
			runLane(t, gc, legs, e.graphStore(), now)
		})
		if !hasMemo(gc.notes, workArmCloseMemo, busy.ID) {
			t.Fatal("a failed listing dropped the leg's memos")
		}
	})
}

// hasMemo reports whether n remembers id under arm, on any leg (the work
// arm's memos are named arm@<leg ref>).
func hasMemo(n wispGCNotes, arm, id string) bool {
	for key := range n.refused {
		if (strings.HasPrefix(key, arm+"\x00") || strings.HasPrefix(key, arm+"@")) && strings.HasSuffix(key, "\x00"+id) {
			return true
		}
	}
	return false
}

// toggleListStore fails List once told to.
type toggleListStore struct {
	batchDeleteWorkStore
	fail bool
}

func (s *toggleListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.fail {
		return nil, errors.New("store unavailable")
	}
	return s.batchDeleteWorkStore.List(q)
}

// Should-fix 5 (memo arms): a held stepless root and a root whose close fails
// are remembered too, so the next pass does not walk them.
func TestWorkArmRemembersHeldAndFailedCloses(t *testing.T) {
	t.Run("held-stepless", func(t *testing.T) {
		store := newBatchDeleteWorkStore()
		root := stepless(t, store, beads.Bead{Title: "attached v1 molecule", Type: "molecule"})
		stepless(t, store, beads.Bead{Title: "live holder", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}})
		e, legs := laneLegs(t, store)
		gc := newWorkMoleculeGC()
		now := time.Now().Add(30 * time.Minute)
		var second string
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			second = runLane(t, gc, legs, e.graphStore(), now)
		})
		if strings.Contains(second, "walked") {
			t.Fatalf("a held stepless root was walked again:\n%s", second)
		}
	})
	t.Run("close-fails", func(t *testing.T) {
		inner := newBatchDeleteWorkStore()
		root := stepless(t, inner, beads.Bead{Title: "junk", Type: "molecule"})
		store := closeFailingStore{batchDeleteWorkStore: inner, failID: root.ID}
		e, legs := laneLegs(t, store)
		gc := newWorkMoleculeGC()
		now := time.Now().Add(30 * time.Minute)
		var second string
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			second = runLane(t, gc, legs, e.graphStore(), now)
		})
		if strings.Contains(second, "walked") {
			t.Fatalf("a root whose close failed was walked again on the next pass:\n%s", second)
		}
	})
}

type closeFailingStore struct {
	batchDeleteWorkStore
	failID string
}

func (s closeFailingStore) Close(id string) error {
	if id == s.failID {
		return errors.New("backend refused the close")
	}
	return s.batchDeleteWorkStore.Close(id)
}

func (s closeFailingStore) SetMetadata(id, key, value string) error {
	if id == s.failID {
		return errors.New("backend refused the write")
	}
	return s.batchDeleteWorkStore.SetMetadata(id, key, value)
}

// hideBdStore hides the BdStore type, so the closure walk takes the per-node
// path over the same store.
type hideBdStore struct{ beads.Store }

// Should-fix 5: on a bd store the level walk (one batched children read per
// level) collects exactly the closure the per-node walk does: ParentID and
// dependency-only children, gc.root_bead_id members and their children,
// ephemeral members, and never a blocker.
func TestClosureLevelWalkMatchesThePerNodeWalk(t *testing.T) {
	fb := beadstest.NewFakeBd()
	bd := beads.NewBdStore("/fake-bd", fb.Run)
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		c, err := bd.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	root := mk(beads.Bead{Title: "root", Type: "molecule"})
	s1 := mk(beads.Bead{Title: "s1", Type: "step", ParentID: root.ID})
	s1a := mk(beads.Bead{Title: "s1a", Type: "step", ParentID: s1.ID})
	m := mk(beads.Bead{Title: "member", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}})
	mc := mk(beads.Bead{Title: "member-child", Type: "step", ParentID: m.ID})
	d := mk(beads.Bead{Title: "dep-only", Type: "step"})
	if err := fb.SeedDep(d.ID, root.ID, "parent-child"); err != nil {
		t.Fatal(err)
	}
	eph := mk(beads.Bead{Title: "eph", Type: "step", ParentID: root.ID, Ephemeral: true})
	ephc := mk(beads.Bead{Title: "eph-child", Type: "step", ParentID: eph.ID, Ephemeral: true})
	blk := mk(beads.Bead{Title: "blocker", Type: "task"})
	if err := fb.SeedDep(s1.ID, blk.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{s1a.ID, s1.ID, mc.ID, m.ID, d.ID, ephc.ID, eph.ID, root.ID} {
		if err := bd.Close(id); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}
	cache := beads.NewCachingStore(bd, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !childrenFollowParentChildDeps(cache) {
		t.Fatal("a cached bd store must take the level walk")
	}
	level, _, err := collectExpiredBeadClosureStatus(cache, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	perNode, _, err := collectExpiredBeadClosureStatus(hideBdStore{cache}, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, b := slices.Clone(level), slices.Clone(perNode)
	sort.Strings(a)
	sort.Strings(b)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("closure sets differ:\nlevel    %v\nper-node %v", a, b)
	}
	if len(a) != 8 {
		t.Fatalf("closure = %v, want the root and its 7 members", a)
	}
	if level[len(level)-1] != root.ID {
		t.Fatalf("root is not last in the level walk's delete order: %v", level)
	}
	if slices.Contains(a, blk.ID) {
		t.Fatal("a blocker was collected into the closure")
	}
}

// Should-fix 5: the narrowed set-level strand guard still refuses a closure
// whose nested root (molecule or workflow) owns an open gc.root_bead_id member
// outside the closure.
func TestPurgeRefusesANestedRootWithAnOpenMember(t *testing.T) {
	for _, nestedType := range []string{"molecule", "task-workflow"} {
		t.Run(nestedType, func(t *testing.T) {
			fb := beadstest.NewFakeBd()
			bd := beads.NewBdStore("/fake-bd", fb.Run)
			mk := func(b beads.Bead) beads.Bead {
				t.Helper()
				c, err := bd.Create(b)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			root := mk(beads.Bead{Title: "root", Type: "molecule"})
			nb := beads.Bead{Title: "nested", Type: "molecule", ParentID: root.ID}
			if nestedType == "task-workflow" {
				nb = beads.Bead{Title: "nested", Type: "task", ParentID: root.ID, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}
			}
			nested := mk(nb)
			open := mk(beads.Bead{Title: "open member of nested", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: nested.ID}})
			for _, id := range []string{nested.ID, root.ID} {
				if err := bd.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			cache := beads.NewCachingStore(bd, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := deleteExpiredBeadClosureFrom(cache, root.ID, nil)
			if !errors.Is(err, errWorkflowDeleteLiveDescendants) {
				t.Fatalf("purge of %s = %v, want a refusal: nested root %s owns open member %s", root.ID, err, nested.ID, open.ID)
			}
			if _, gerr := bd.Get(nested.ID); gerr != nil {
				t.Fatalf("nested root %s was deleted", nested.ID)
			}
		})
	}
}

// Review: a closure member another process reopened while this cache still
// holds it closed is live work. The purge must see it as the backing does: a
// list through the cache drops a backing row the cache holds closed, or
// answers with the cached closed row, so the purge would delete the step with
// its closure or strand it under a deleted root. Both walks: the per-node walk
// and the bd level walk.
func TestPurgeRefusesAMemberReopenedBehindTheCache(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backing func() beads.Store
	}{
		{name: "per-node walk", backing: func() beads.Store { return beads.NewMemStore() }},
		{name: "bd level walk", backing: func() beads.Store { return beads.NewBdStore("/fake-bd", beadstest.NewFakeBd().Run) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := tc.backing()
			cache := beads.NewCachingStore(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			root, err := cache.Create(beads.Bead{Title: "v1 poured", Type: "molecule"})
			if err != nil {
				t.Fatal(err)
			}
			step, err := cache.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{root.ID, step.ID} {
				if err := cache.Close(id); err != nil {
					t.Fatal(err)
				}
			}
			// Another process puts the step back to work; this cache has not
			// seen it.
			if err := backing.Reopen(step.ID); err != nil {
				t.Fatal(err)
			}
			if cached, err := cache.Get(step.ID); err != nil || cached.Status != "closed" {
				t.Fatalf("precondition: the cache holds step %s as %q (%v), want closed", step.ID, cached.Status, err)
			}
			err = deleteExpiredBeadClosureFrom(cache, root.ID, nil)
			if !errors.Is(err, errWorkflowDeleteLiveDescendants) {
				t.Fatalf("purge of %s = %v, want a refusal: its step %s is open in the backing", root.ID, err, step.ID)
			}
			for _, id := range []string{root.ID, step.ID} {
				if _, err := backing.Get(id); err != nil {
					t.Errorf("%s was deleted: %v", id, err)
				}
			}
		})
	}
}

// Must-fix 1, the two halves of the arm sharing. With two walks, a close that
// goes first leaves one for the purge: the expired closure is purged on the
// first visit even behind abandoned roots. With one walk, the arms alternate:
// over two visits each arm gets a walk.
func TestWorkArmArmsShareTheBudget(t *testing.T) {
	setup := func(t *testing.T) (batchDeleteWorkStore, []beads.Bead, beads.Bead, splitEnv, WorkLegs) {
		store := newBatchDeleteWorkStore()
		var junk []beads.Bead
		for i := 0; i < 3; i++ {
			junk = append(junk, stepless(t, store, beads.Bead{Title: fmt.Sprintf("junk-%d", i), Type: "molecule"}))
		}
		expired := closedPoured(t, store, "expired")
		e, legs := laneLegs(t, store)
		return store, junk, expired, e, legs
	}
	closedJunk := func(store beads.Store, junk []beads.Bead) int {
		n := 0
		for _, j := range junk {
			if b, err := store.Get(j.ID); err == nil && b.Status == "closed" {
				n++
			}
		}
		return n
	}
	t.Run("reserve-with-two-walks", func(t *testing.T) {
		store, junk, expired, e, legs := setup(t)
		withWorkArmBudget(t, 2, func() {
			enforcedLane(t, func() { runLane(t, newWorkMoleculeGC(), legs, e.graphStore(), time.Now().Add(2*time.Hour)) })
		})
		if !laneGone(store, expired.ID) {
			t.Fatal("with two walks, a close that went first left none for the purge")
		}
		if closedJunk(store, junk) != 1 {
			t.Fatalf("closed %d abandoned root(s), want 1 (the other walk is the purge's)", closedJunk(store, junk))
		}
	})
	t.Run("alternation-with-one-walk", func(t *testing.T) {
		_, _, _, e, legs := setup(t)
		gc := newWorkMoleculeGC()
		var first, second string
		withWorkArmBudget(t, 1, func() {
			enforcedLane(t, func() {
				first = runLane(t, gc, legs, e.graphStore(), time.Now().Add(2*time.Hour))
				second = runLane(t, gc, legs, e.graphStore(), time.Now().Add(2*time.Hour))
			})
		})
		// One visit closes (the close goes first), the next purges (the
		// purge goes first): a closure is purged on the second visit even
		// though abandoned roots remain for the close.
		if !strings.Contains(first, "closed abandoned root") || !strings.Contains(second, "purged 1 closure") {
			t.Fatalf("over two one-walk visits each arm must get a walk:\nfirst:\n%s\nsecond:\n%s", first, second)
		}
	})
}
