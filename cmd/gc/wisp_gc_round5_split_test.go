package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Round-5 review: the work arm under a wall deadline that cuts leg visits,
// the first walk of a pass, per-leg memo pruning, and the cursors. The wall
// clock is a fake the slow stores below advance, so no test sleeps.

// laneClock is a fake wall clock for the work arm's budget.
type laneClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *laneClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *laneClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// useLaneClock makes the work arm's budget read a fake clock, and sets its
// wall budget.
func useLaneClock(t *testing.T, wall time.Duration) *laneClock {
	t.Helper()
	c := &laneClock{now: time.Now()}
	prevClock, prevWall := wispGCClock, wispGCWorkArmWallBudget
	wispGCClock, wispGCWorkArmWallBudget = c.read, wall
	t.Cleanup(func() { wispGCClock, wispGCWorkArmWallBudget = prevClock, prevWall })
	return c
}

// clockedStore is a work ledger whose calls cost fake wall time: each List
// advances the clock by perList (when armed), each batch delete by perDelete;
// onDelete runs after each batch delete.
type clockedStore struct {
	batchDeleteWorkStore
	clock     *laneClock
	perList   *time.Duration
	perDelete time.Duration
	onDelete  func()
}

func newClockedStore(clock *laneClock, perList time.Duration) clockedStore {
	return clockedStore{batchDeleteWorkStore: newBatchDeleteWorkStore(), clock: clock, perList: &perList}
}

func (s clockedStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.clock.advance(*s.perList)
	return s.batchDeleteWorkStore.List(q)
}

func (s clockedStore) DeleteBatch(ids []string) error {
	err := s.batchDeleteWorkStore.DeleteBatch(ids)
	s.clock.advance(s.perDelete)
	if s.onDelete != nil {
		s.onDelete()
	}
	return err
}

func legIndex(t *testing.T, legs WorkLegs, ref string) int {
	t.Helper()
	i, at := 0, -1
	legs.eachLeg(func(r string, _ beads.Store) {
		if r == ref {
			at = i
		}
		i++
	})
	if at < 0 {
		t.Fatalf("no leg %s", ref)
	}
	return at
}

func countClosed(store beads.Store, roots []beads.Bead) int {
	n := 0
	for _, r := range roots {
		if b, err := store.Get(r.ID); err != nil || b.Status == "closed" {
			n++
		}
	}
	return n
}

func countGone(store beads.Store, roots []beads.Bead) int {
	n := 0
	for _, r := range roots {
		if laneGone(store, r.ID) {
			n++
		}
	}
	return n
}

// Must-fix 1: a visit the deadline cuts before either arm walks is not a
// turn. Legs city, P, L. A pass starting at the city spends every walk
// there. A pass starting at P walks P's one fresh root, then lists L past
// the deadline: L is visited but cut. A pass starting at L walks once (the
// first walk ignores the deadline). If the cut visit flipped L's arm order,
// L's walking visits would always start with the same arm and the other
// would starve.
func TestWorkArmArmOrderSurvivesCutVisits(t *testing.T) {
	clock := useLaneClock(t, 80*time.Millisecond)
	p := newClockedStore(clock, 0)
	l := newClockedStore(clock, 100*time.Millisecond)
	var junk, expired []beads.Bead
	for i := 0; i < 15; i++ {
		junk = append(junk, stepless(t, l, beads.Bead{Title: fmt.Sprintf("junk-%02d", i), Type: "molecule"}))
		expired = append(expired, closedPoured(t, l, fmt.Sprintf("exp-%02d", i)))
	}
	e, legs := laneLegs(t, p, l)
	for i := 0; i < 200; i++ {
		stepless(t, e.work, beads.Bead{Title: fmt.Sprintf("cityjunk-%03d", i), Type: "molecule"})
	}
	pAt, lAt := legIndex(t, legs, "rig:rig-A"), legIndex(t, legs, "rig:rig-B")
	if pAt != 1 || lAt != 2 {
		t.Fatalf("leg order: P at %d, L at %d; want 1 and 2", pAt, lAt)
	}
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	cutVisits := 0
	withWorkArmBudget(t, 5, func() {
		enforcedLane(t, func() {
			for i := 0; i < 30; i++ {
				if i%3 == pAt {
					stepless(t, p, beads.Bead{Title: fmt.Sprintf("p-fresh-%02d", i), Type: "molecule"})
				}
				before := countClosed(l, junk) + countGone(l, expired) + countGone(l, junk)
				runLane(t, gc, legs, e.graphStore(), now)
				if i%3 == pAt && countClosed(l, junk)+countGone(l, expired)+countGone(l, junk) == before {
					cutVisits++
				}
			}
		})
	})
	// A closed junk root is past retention too, so the purge may take it
	// rather than an expired one: a gone junk root was closed AND purged.
	closes := countClosed(l, junk)
	purges := countGone(l, expired) + countGone(l, junk)
	if cutVisits < 5 {
		t.Fatalf("the fixture did not cut L's visits (%d cut); it tests nothing", cutVisits)
	}
	if closes == 0 || purges == 0 {
		t.Fatalf("one arm on leg L starved over 10 walking visits: %d close(s), %d purge(s)", closes, purges)
	}
	if closes+purges != 10 {
		t.Fatalf("L walked %d time(s) in its 10 walking visits; want 10", closes+purges)
	}
}

// Should-fix 4: the deadline never stops a pass's first walk, so a leg
// whose listing alone outlasts the wall budget still makes progress.
func TestWorkArmFirstWalkIgnoresTheDeadline(t *testing.T) {
	clock := useLaneClock(t, 100*time.Millisecond)
	slow := newClockedStore(clock, 40*time.Millisecond)
	var expired []beads.Bead
	for i := 0; i < 3; i++ {
		expired = append(expired, closedPoured(t, slow, fmt.Sprintf("exp-%d", i)))
	}
	e, legs := laneLegs(t, slow)
	gc := newWorkMoleculeGC()
	withWorkArmBudget(t, 5, func() {
		enforcedLane(t, func() {
			for pass := 1; pass <= 3; pass++ {
				runLane(t, gc, legs, e.graphStore(), time.Now().Add(2*time.Hour))
				if got := countGone(slow, expired); got != pass {
					t.Fatalf("after pass %d, %d closure(s) purged; want one per pass (the first walk is not stopped by the deadline, the second is)", pass, got)
				}
			}
		})
	})
}

// Should-fix 4: a pass that ends with no walk while legs were left unlisted
// says so.
func TestWorkArmLogsAPassThatEndsWithoutAWalk(t *testing.T) {
	clock := useLaneClock(t, 80*time.Millisecond)
	dark := newClockedStore(clock, 100*time.Millisecond)
	busy := newClockedStore(clock, 0)
	root := closedPoured(t, busy, "exp")
	e, legs := laneLegs(t, dark, busy)
	gc := newWorkMoleculeGC()
	var logs []string
	enforcedLane(t, func() {
		for i := 0; i < 3; i++ {
			logs = append(logs, runLane(t, gc, legs, e.graphStore(), time.Now().Add(2*time.Hour)))
		}
	})
	if !strings.Contains(logs[1], "work arm pass ended with no walk") || !strings.Contains(logs[1], "after listing 1 of 3 leg(s)") {
		t.Fatalf("a pass that listed only the dark leg and walked nothing logged nothing; got %q", logs[1])
	}
	if strings.Contains(logs[2], "ended with no walk") || !laneGone(busy, root.ID) {
		t.Fatalf("the pass starting at the busy leg did not walk it; log %q", logs[2])
	}
}

// Should-fix 6: take obeys ctx and the deadline, the first walk excepted
// from the deadline only, and the reserve.
func TestWispGCBudgetTake(t *testing.T) {
	clock := useLaneClock(t, time.Second)
	t.Run("deadline-mid-pass", func(t *testing.T) {
		b := &wispGCBudget{walks: 5, deadline: clock.read().Add(time.Second)}
		for i := 0; i < 2; i++ {
			if !b.take() {
				t.Fatalf("take %d before the deadline refused", i+1)
			}
		}
		clock.advance(time.Second)
		if b.take() {
			t.Fatal("a take past the deadline, after the first walk, was granted")
		}
		if !b.stopped() {
			t.Fatal("a budget past its deadline does not report stopped")
		}
	})
	t.Run("first-walk-past-the-deadline", func(t *testing.T) {
		b := &wispGCBudget{walks: 5, deadline: clock.read().Add(-time.Second)}
		if !b.take() {
			t.Fatal("the first walk of a pass was stopped by the deadline")
		}
		if b.take() {
			t.Fatal("a second walk past the deadline was granted")
		}
	})
	t.Run("ctx-mid-pass", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		b := &wispGCBudget{ctx: ctx, walks: 5}
		if !b.take() {
			t.Fatal("a take before ctx was done refused")
		}
		cancel()
		if b.take() {
			t.Fatal("a take after ctx was done was granted")
		}
	})
	t.Run("ctx-stops-the-first-walk", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if (&wispGCBudget{ctx: ctx, walks: 5}).take() {
			t.Fatal("a first walk after ctx was done was granted")
		}
	})
	t.Run("reserve", func(t *testing.T) {
		b := &wispGCBudget{walks: 2, reserve: 1}
		if !b.take() || b.take() {
			t.Fatal("the reserve was not left untouched")
		}
	})
}

// Should-fix 6: a pass that stops mid-pass, on the deadline or on ctx,
// starts no second walk and lists no further leg.
func TestWorkArmStopsMidPass(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		clock := useLaneClock(t, 80*time.Millisecond)
		leg := newClockedStore(clock, 0)
		leg.perDelete = 100 * time.Millisecond
		var expired []beads.Bead
		for i := 0; i < 3; i++ {
			expired = append(expired, closedPoured(t, leg, fmt.Sprintf("exp-%d", i)))
		}
		e, legs := laneLegs(t, leg)
		gc := newWorkMoleculeGC()
		gc.legCursor = legIndex(t, legs, "rig:rig-A")
		withWorkArmBudget(t, 5, func() {
			enforcedLane(t, func() { runLane(t, gc, legs, e.graphStore(), time.Now().Add(2*time.Hour)) })
		})
		if got := countGone(leg, expired); got != 1 {
			t.Fatalf("a pass whose first walk ran past the deadline purged %d closure(s); want 1", got)
		}
	})
	t.Run("ctx", func(t *testing.T) {
		clock := useLaneClock(t, time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		leg := newClockedStore(clock, 0)
		leg.onDelete = cancel
		var expired []beads.Bead
		for i := 0; i < 3; i++ {
			expired = append(expired, closedPoured(t, leg, fmt.Sprintf("exp-%d", i)))
		}
		lists := 0
		var mu sync.Mutex
		next := wispLaneListCounter{batchDeleteWorkStore: newBatchDeleteWorkStore(), mu: &mu, lists: &lists}
		closedPoured(t, next, "next-exp")
		e, legs := laneLegs(t, leg, next)
		gc := newWorkMoleculeGC()
		gc.legCursor = legIndex(t, legs, "rig:rig-A")
		withWorkArmBudget(t, 5, func() {
			enforcedLane(t, func() {
				withCloseAbandonedTTL(t, 5*time.Minute, func() {
					_, _ = gc.pass(ctx, legs, e.graphStore(), time.Hour, time.Now().Add(2*time.Hour))
				})
			})
		})
		if got := countGone(leg, expired); got != 1 {
			t.Fatalf("a pass whose ctx was done during its first walk purged %d closure(s); want 1", got)
		}
		if lists != 0 {
			t.Fatalf("a pass whose ctx was done listed the next leg %d time(s)", lists)
		}
	})
}

// Should-fix 6: memos are pruned per leg, against that leg's own listing: a
// pass that lists only some legs prunes those, and keeps the rest.
func TestWorkArmPrunesMemosPerLeg(t *testing.T) {
	clock := useLaneClock(t, 80*time.Millisecond)
	a := newClockedStore(clock, 0)
	b := newClockedStore(clock, 0)
	mkBusy := func(s beads.Store, name string) (beads.Bead, beads.Bead) {
		root := stepless(t, s, beads.Bead{Title: name, Type: "molecule"})
		step := stepless(t, s, beads.Bead{Title: "live step", Type: "step", ParentID: root.ID})
		return root, step
	}
	aRoot, aStep := mkBusy(a, "busy-a")
	bRoot, bStep := mkBusy(b, "busy-b")
	e, legs := laneLegs(t, a, b)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(30 * time.Minute)
	withWorkArmBudget(t, 5, func() {
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now) // starts at the city; walks both busy roots
			if !legMemo(gc, "rig:rig-A", aRoot.ID) || !legMemo(gc, "rig:rig-B", bRoot.ID) {
				t.Fatal("the busy roots' outcomes were not remembered")
			}
			for _, x := range []beads.Bead{aStep, aRoot} {
				if err := a.Delete(x.ID); err != nil {
					t.Fatal(err)
				}
			}
			for _, x := range []beads.Bead{bStep, bRoot} {
				if err := b.Delete(x.ID); err != nil {
					t.Fatal(err)
				}
			}
			*a.perList = 100 * time.Millisecond
			runLane(t, gc, legs, e.graphStore(), now) // starts at A; its listing spends the wall budget
		})
	})
	if legMemo(gc, "rig:rig-A", aRoot.ID) {
		t.Fatal("the listed leg's memo of a deleted root was kept")
	}
	if !legMemo(gc, "rig:rig-B", bRoot.ID) {
		t.Fatal("an unlisted leg's memo was pruned")
	}
}

// legMemo reports whether the work arm remembers id's close outcome on leg ref.
func legMemo(gc *workMoleculeGC, ref, id string) bool {
	_, ok := gc.notes.refused[workArmMemo(workArmCloseMemo, ref)+"\x00"+id]
	return ok
}

// Should-fix 3: the close arm resumes after its cursor, so 20 busy roots
// listed ahead of an abandoned one cannot hold it off forever.
func TestWorkArmCloseCursorReachesTheLastRoot(t *testing.T) {
	store := newBatchDeleteWorkStore()
	for i := 0; i < 20; i++ {
		busy := stepless(t, store, beads.Bead{Title: fmt.Sprintf("busy-%02d", i), Type: "molecule"})
		stepless(t, store, beads.Bead{Title: "live", Type: "step", ParentID: busy.ID})
	}
	junk := stepless(t, store, beads.Bead{Title: "abandoned", Type: "molecule"})
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	base := time.Now().Add(2 * time.Hour)
	passes := 0
	withWorkArmBudget(t, 1, func() {
		enforcedLane(t, func() {
			for passes = 1; passes <= 100; passes++ {
				runLane(t, gc, legs, e.graphStore(), base.Add(time.Duration(passes)*20*time.Minute))
				if b, _ := store.Get(junk.ID); b.Status == "closed" {
					break
				}
			}
		})
	})
	if b, _ := store.Get(junk.ID); b.Status != "closed" {
		t.Fatal("the abandoned root behind 20 busy roots was never closed in 100 passes")
	}
}

// Nit: the close arm's second turn in a visit does not walk a root its first
// turn walked (closeWalked), and each arm keeps its own cursor.
func TestWorkArmCloseWalksARootOnceAndArmsKeepTheirCursors(t *testing.T) {
	store := newBatchDeleteWorkStore()
	j1 := stepless(t, store, beads.Bead{Title: "junk-1", Type: "molecule"})
	j2 := stepless(t, store, beads.Bead{Title: "junk-2", Type: "molecule"})
	c1 := closedPoured(t, store, "exp-1")
	c2 := closedPoured(t, store, "exp-2")
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	gc.legCursor = legIndex(t, legs, "rig:rig-A")
	var out string
	withWorkArmBudget(t, 5, func() {
		enforcedLane(t, func() { out = runLane(t, gc, legs, e.graphStore(), time.Now().Add(2*time.Hour)) })
	})
	if !strings.Contains(out, "work arm walked 4 root(s)") {
		t.Fatalf("want 4 walks (two closes, two purges, no root walked twice); log:\n%s", out)
	}
	closeAt, purgeAt := gc.rootCursor["rig:rig-A\x00close"], gc.rootCursor["rig:rig-A\x00purge"]
	lastJunk, lastExp := max(j1.ID, j2.ID), max(c1.ID, c2.ID)
	if closeAt != lastJunk || purgeAt != lastExp {
		t.Fatalf("cursors close=%q purge=%q; want close at the last junk root %q and purge at the last expired root %q", closeAt, purgeAt, lastJunk, lastExp)
	}
}

// Nit: a per-root line the work arm printed is not printed again just
// because a budgeted pass did not reach the root; it ages out after the
// recheck window.
func TestWispGCNotesKeepDetailsAcrossPartialPasses(t *testing.T) {
	var n wispGCNotes
	now := time.Now()
	if !n.detail("k") {
		t.Fatal("a new line was not printed")
	}
	n.endPartialPass(now)
	n.endPartialPass(now.Add(time.Minute)) // a pass that did not reach the root
	if n.detail("k") {
		t.Fatal("a line was printed again after one pass skipped its root")
	}
	n.endPartialPass(now.Add(2 * time.Minute))
	n.endPartialPass(now.Add(2*time.Minute + wispGCRefusalRecheck + time.Second))
	if !n.detail("k") {
		t.Fatal("a line unseen for longer than the recheck window was never printed again")
	}
}

// Nit: the leg cursor moves by one leg a pass. With four legs a cursor that
// moved by two would visit only half of them first.
func TestWorkArmLegCursorVisitsEveryLegOfFour(t *testing.T) {
	// A and B hold more work than the passes can walk, so a pass that starts
	// at either spends its walk there: C is walked only by a pass that
	// starts at C.
	legA, legB, legC := newBatchDeleteWorkStore(), newBatchDeleteWorkStore(), newBatchDeleteWorkStore()
	var bs []beads.Bead
	for i := 0; i < 4; i++ {
		closedPoured(t, legA, fmt.Sprintf("a-%d", i))
		bs = append(bs, closedPoured(t, legB, fmt.Sprintf("b-%d", i)))
	}
	c := closedPoured(t, legC, "c")
	e, legs := laneLegs(t, legA, legB, legC)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	withWorkArmBudget(t, 1, func() {
		enforcedLane(t, func() {
			for i := 0; i < 4; i++ {
				runLane(t, gc, legs, e.graphStore(), now)
			}
		})
	})
	if countGone(legB, bs) == 0 || !laneGone(legC, c.ID) {
		t.Fatalf("after four one-walk passes over four legs, B purged %d, C gone=%v: the leg cursor skipped a leg", countGone(legB, bs), laneGone(legC, c.ID))
	}
}

// Nit: workLegs reads the published config snapshot, not cr.cfg, so the lane
// goroutine does not race a reload (run under -race), and it sees what the
// reload published.
func TestWorkLegsReadsThePublishedConfig(t *testing.T) {
	cr, _ := laneRuntime(t, true)
	rigs := func(l WorkLegs) int {
		n := 0
		l.eachLeg(func(ref string, _ beads.Store) {
			if strings.HasPrefix(ref, "rig:") {
				n++
			}
		})
		return n
	}
	if rigs(cr.workLegs()) != 1 {
		t.Fatal("the fixture's rig is not a leg")
	}
	noRigs := *cr.serviceConfigSnapshot()
	noRigs.Rigs = nil
	// A reader and a reload interleave many times, the reload publishing
	// under serviceStateMu as publishRuntimeConfig does: a workLegs that read
	// cr.cfg outside that lock races it.
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 2000; i++ {
			_ = cr.workLegs()
			runtime.Gosched()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 2000; i++ {
			next := noRigs
			cr.serviceStateMu.Lock()
			cr.cfg = &next
			cr.serviceStateMu.Unlock()
			runtime.Gosched()
		}
	}()
	close(start)
	wg.Wait()
	if got := rigs(cr.workLegs()); got != 0 {
		t.Fatalf("workLegs saw %d rig leg(s) after a reload published a config with none", got)
	}
}

// ctxListStore cancels its ctx on its first List, and counts its Lists.
type ctxListStore struct {
	batchDeleteWorkStore
	cancel context.CancelFunc
	lists  *int
}

func (s ctxListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	*s.lists++
	s.cancel()
	return s.batchDeleteWorkStore.List(q)
}

// Should-fix 5: a stop during a leg's open listing does not start its closed
// listing.
func TestWorkArmStopsBetweenListings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lists := 0
	leg := ctxListStore{batchDeleteWorkStore: newBatchDeleteWorkStore(), cancel: cancel, lists: &lists}
	closedPoured(t, leg, "exp")
	e, legs := laneLegs(t, leg)
	gc := newWorkMoleculeGC()
	gc.legCursor = legIndex(t, legs, "rig:rig-A")
	enforcedLane(t, func() {
		_, _ = gc.pass(ctx, legs, e.graphStore(), time.Hour, time.Now().Add(2*time.Hour))
	})
	// The open listing is one List per status (open, in_progress); the
	// closed listing would be a third.
	if lists != 2 {
		t.Fatalf("a pass stopped during a leg's open listing made %d List call(s) on it; want 2 (the closed listing must not start)", lists)
	}
}

// Nit: through the lane, a dry-run per-root line is not repeated on the pass
// that next reaches its root after a pass that did not.
func TestWorkArmDoesNotRelogARootAPassSkipped(t *testing.T) {
	store := newBatchDeleteWorkStore()
	x := stepless(t, store, beads.Bead{Title: "junk-x", Type: "molecule"})
	y := stepless(t, store, beads.Bead{Title: "junk-y", Type: "molecule"})
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	var logs []string
	withWorkArmBudget(t, 1, func() {
		withWorkMoleculesEnforced(t, func() { // the close itself stays dry-run
			for i := 0; i < 3; i++ {
				gc.legCursor = legIndex(t, legs, "rig:rig-A")
				logs = append(logs, runLane(t, gc, legs, e.graphStore(), now))
			}
		})
	})
	line := func(id string) string { return "molecule root " + id + " would be closed" }
	if !strings.Contains(logs[0], line(x.ID)) || !strings.Contains(logs[1], line(y.ID)) {
		t.Fatalf("the fixture did not walk x then y; logs:\n%s\n--\n%s", logs[0], logs[1])
	}
	if strings.Contains(logs[2], line(x.ID)) {
		t.Fatalf("x's line was repeated because the pass before did not reach x:\n%s", logs[2])
	}
}
