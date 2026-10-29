package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// The work arm on its lane: one budget per pass shared across legs, rotating
// cursors, remembered outcomes. Every row runs the arm the way its lane does
// (workMoleculeGC.pass, state kept across passes) over a split city whose rig
// leg is a work ledger that can batch-delete, as bd can.

// laneLegs is the split env with its rig leg replaced by stores (one leg each,
// rig-A, rig-B, ... when more than one is given).
func laneLegs(t *testing.T, stores ...beads.Store) (splitEnv, WorkLegs) {
	t.Helper()
	e := newSplitEnvWithRig(t, true)
	rigs := map[string]beads.Store{}
	base := e.cfg.Rigs[0]
	e.cfg.Rigs = nil
	for i, s := range stores {
		r := base
		r.Name = fmt.Sprintf("rig-%c", 'A'+i)
		r.Path = base.Path + "-" + r.Name
		e.cfg.Rigs = append(e.cfg.Rigs, r)
		rigs[r.Name] = s
	}
	return e, workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), rigs)
}

func runLane(t *testing.T, gc *workMoleculeGC, legs WorkLegs, graph beads.Store, now time.Time) string {
	t.Helper()
	var logs bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	defer func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) }()
	withCloseAbandonedTTL(t, 5*time.Minute, func() {
		if _, err := gc.pass(context.Background(), legs, graph, time.Hour, now); err != nil {
			t.Logf("pass: %v", err)
		}
	})
	return logs.String()
}

func enforcedLane(t *testing.T, fn func()) {
	t.Helper()
	withWorkMoleculesEnforced(t, func() { withCloseAbandonedEnforced(t, fn) })
}

func closedPoured(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	root := stepless(t, store, beads.Bead{Title: title, Type: "molecule"})
	step := stepless(t, store, beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
	for _, id := range []string{step.ID, root.ID} {
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := store.Get(root.ID)
	return got
}

func laneGone(store beads.Store, id string) bool {
	_, err := store.Get(id)
	return errors.Is(err, beads.ErrNotFound)
}

// Must-fix 1: busy molecules (live steps) idle past the TTL do not take every
// walk of every pass: their "not closable" outcome is remembered, so the
// abandoned root behind them is reached.
func TestWorkArmCloseIsNotStarvedByBusyMolecules(t *testing.T) {
	store := newBatchDeleteWorkStore()
	for i := 0; i < 5; i++ {
		busy := stepless(t, store, beads.Bead{Title: fmt.Sprintf("busy-%d", i), Type: "molecule"})
		stepless(t, store, beads.Bead{Title: "live step", Type: "step", ParentID: busy.ID})
	}
	junk := stepless(t, store, beads.Bead{Title: "abandoned", Type: "molecule"})
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(30 * time.Minute)
	enforcedLane(t, func() {
		for i := 0; i < 3 && !strings.EqualFold(laneGet(t, store, junk.ID).Status, "closed"); i++ {
			runLane(t, gc, legs, e.graphStore(), now)
		}
	})
	if got := laneGet(t, store, junk.ID); got.Status != "closed" {
		t.Fatalf("abandoned root %s still %q after 3 passes behind 5 busy molecules (budget %d)", junk.ID, got.Status, wispGCWorkArmWalkBudget)
	}
}

func laneGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// laneFailingDeleteStore refuses every delete and records which roots it was asked
// to delete.
type laneFailingDeleteStore struct {
	batchDeleteWorkStore
	asked *[]string
}

func (s laneFailingDeleteStore) DeleteBatch(ids []string) error {
	*s.asked = append(*s.asked, ids...)
	return errors.New("backend refused the delete")
}

// Must-fix 1: a delete that keeps failing is remembered, so it does not take
// a walk every pass: the next pass walks the next root.
func TestWorkArmRemembersAFailingDelete(t *testing.T) {
	var asked []string
	inner := newBatchDeleteWorkStore()
	store := laneFailingDeleteStore{batchDeleteWorkStore: inner, asked: &asked}
	a := closedPoured(t, inner, "a")
	b := closedPoured(t, inner, "b")
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	withWorkArmBudget(t, 1, func() {
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			runLane(t, gc, legs, e.graphStore(), now)
		})
	})
	if !strings.Contains(strings.Join(asked, ","), a.ID) || !strings.Contains(strings.Join(asked, ","), b.ID) {
		t.Fatalf("delete attempts = %v: with a budget of 1, the failed root must be remembered so the second pass reaches the other", asked)
	}
}

// Must-fix 2(a), purge_cap_off: one budget per pass across every leg, and the
// cursors carry the rest to later passes.
func TestWorkArmBudgetIsSharedAcrossLegs(t *testing.T) {
	legA, legB := newBatchDeleteWorkStore(), newBatchDeleteWorkStore()
	var all []beads.Bead
	for i := 0; i < 4; i++ {
		all = append(all, closedPoured(t, legA, fmt.Sprintf("a-%d", i)))
	}
	for i := 0; i < 4; i++ {
		all = append(all, closedPoured(t, legB, fmt.Sprintf("b-%d", i)))
	}
	count := func() int {
		n := 0
		for i, r := range all {
			store := beads.Store(legA)
			if i >= 4 {
				store = legB
			}
			if laneGone(store, r.ID) {
				n++
			}
		}
		return n
	}
	e, legs := laneLegs(t, legA, legB)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	withWorkArmBudget(t, 3, func() {
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			if n := count(); n != 3 {
				t.Fatalf("after one pass %d closure(s) purged across both legs, want exactly the shared budget of 3", n)
			}
			runLane(t, gc, legs, e.graphStore(), now)
			runLane(t, gc, legs, e.graphStore(), now)
		})
	})
	if n := count(); n != 8 {
		t.Fatalf("after three passes %d of 8 closures purged; the cursors must carry the rest across legs", n)
	}
}

// close_cap_off: the abandoned close spends the same budget.
func TestWorkArmCloseSpendsTheBudget(t *testing.T) {
	store := newBatchDeleteWorkStore()
	var junk []beads.Bead
	for i := 0; i < 5; i++ {
		junk = append(junk, stepless(t, store, beads.Bead{Title: fmt.Sprintf("junk-%d", i), Type: "molecule"}))
	}
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	withWorkArmBudget(t, 2, func() {
		enforcedLane(t, func() { runLane(t, gc, legs, e.graphStore(), time.Now().Add(30*time.Minute)) })
	})
	closed := 0
	for _, j := range junk {
		if laneGet(t, store, j.ID).Status == "closed" {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("one pass closed %d abandoned root(s), want the budget of 2", closed)
	}
}

// Must-fix 3 cap_charges_refusal: a remembered refusal costs no walk, so with a
// budget of 1 the pass after a refusal reaches the next root.
func TestWorkArmRememberedRefusalCostsNoWalk(t *testing.T) {
	store := newBatchDeleteWorkStore()
	held := closedPoured(t, store, "a-held")
	kids, _ := store.Children(held.ID, beads.IncludeClosed)
	inProgress := "in_progress"
	if err := store.Update(kids[0].ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	held = laneGet(t, store, held.ID)
	ok := closedPoured(t, store, "b-ok")
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	withWorkArmBudget(t, 1, func() {
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now) // refuses a-held
			gc.rootCursor = map[string]string{}       // restart the rotation at a-held
			runLane(t, gc, legs, e.graphStore(), now) // a-held remembered: b-ok walked
		})
	})
	if !laneGone(store, ok.ID) {
		t.Fatal("the remembered refusal took the only walk; b-ok was not purged")
	}
	if laneGone(store, held.ID) {
		t.Fatal("the held closure was purged")
	}
}

// memo_ignore_updated and memo_ignore_until: a refusal is re-walked as soon as
// its root changes, or once the recheck expires; never before.
func TestWorkArmRefusalIsRewalkedOnChangeOrExpiry(t *testing.T) {
	setup := func(t *testing.T) (beads.Store, beads.Bead, beads.Bead, *workMoleculeGC, splitEnv, WorkLegs) {
		store := newBatchDeleteWorkStore()
		root := closedPoured(t, store, "held")
		kids, _ := store.Children(root.ID, beads.IncludeClosed)
		inProgress := "in_progress"
		if err := store.Update(kids[0].ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
		e, legs := laneLegs(t, store)
		return store, root, kids[0], newWorkMoleculeGC(), e, legs
	}
	now := time.Now().Add(2 * time.Hour)
	t.Run("root-changed", func(t *testing.T) {
		store, root, step, gc, e, legs := setup(t)
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			if err := store.Close(step.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadata(root.ID, "touched", "1"); err != nil {
				t.Fatal(err)
			}
			runLane(t, gc, legs, e.graphStore(), now)
		})
		if !laneGone(store, root.ID) {
			t.Fatal("a refused root that changed was not re-walked")
		}
	})
	t.Run("recheck-expired", func(t *testing.T) {
		store, root, step, gc, e, legs := setup(t)
		enforcedLane(t, func() {
			runLane(t, gc, legs, e.graphStore(), now)
			if err := store.Close(step.ID); err != nil {
				t.Fatal(err)
			}
			runLane(t, gc, legs, e.graphStore(), now)
			if laneGone(store, root.ID) {
				t.Fatal("an unchanged refused root was re-walked before its recheck")
			}
			runLane(t, gc, legs, e.graphStore(), now.Add(2*wispGCRefusalRecheck))
		})
		if !laneGone(store, root.ID) {
			t.Fatal("a refused root was not re-walked after its recheck expired")
		}
	})
}

// age_created and age_dry_created: the work arm ages a closed root on its
// close (last update), in both the enforced and the dry pass.
func TestWorkArmAgesRootsOnTheirClose(t *testing.T) {
	now := time.Now()
	seed := []beads.Bead{
		{ID: "gc-1", Title: "long-lived, just closed", Type: "molecule", Status: "closed", CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now},
		{ID: "gc-2", Title: "idle junk", Type: "molecule", Status: "open", CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-72 * time.Hour)},
	}
	store := batchDeleteWorkStore{MemStore: beads.NewMemStoreFrom(10, seed, nil)}
	e, legs := laneLegs(t, store)
	out := runLane(t, newWorkMoleculeGC(), legs, e.graphStore(), now)
	if !strings.Contains(out, "0 closed root(s) past retention") {
		t.Errorf("dry pass counted a root closed just now as past retention:\n%s", out)
	}
	withWorkMoleculesEnforced(t, func() { runLane(t, newWorkMoleculeGC(), legs, e.graphStore(), now) })
	if laneGone(store, "gc-1") {
		t.Fatal("a long-lived root closed just now was purged: the clock is its creation, not its close")
	}
}

// log_detail_always and log_endpass_noop: a per-root dry-run line is printed
// when it is new, not on every pass.
func TestWispGCDryRunDetailIsPrintedOnlyOnChange(t *testing.T) {
	store := newGCStore([]beads.Bead{{ID: "wisp-leaked", Status: "open", Type: "molecule", CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour), Metadata: map[string]string{"gc.kind": "wisp"}}})
	wg := newWispGC(5*time.Minute, time.Hour, 0)
	pass := func() string {
		var out string
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			out = captureWispGCLog(t, func() {
				if _, err := wg.runGC(nil, beads.GraphStore{Store: store}, beads.SessionStore{}, beads.MailStore{Store: store}, time.Now()); err != nil {
					t.Fatal(err)
				}
			})
		})
		return out
	}
	if first := pass(); !strings.Contains(first, "wisp-leaked would be closed") {
		t.Fatalf("first pass did not report the root: %q", first)
	}
	if second := pass(); strings.Contains(second, "wisp-leaked would be closed") {
		t.Fatalf("second pass reprinted an unchanged root: %q", second)
	}
}

// Nit: a refused root's detail line is not reprinted when the recheck re-walks
// it and finds the same outcome.
func TestWorkArmRefusalDetailIsNotReprintedOnRecheck(t *testing.T) {
	store := newBatchDeleteWorkStore()
	root := closedPoured(t, store, "held")
	kids, _ := store.Children(root.ID, beads.IncludeClosed)
	inProgress := "in_progress"
	if err := store.Update(kids[0].ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	var first, again string
	enforcedLane(t, func() {
		first = runLane(t, gc, legs, e.graphStore(), now)
		again = runLane(t, gc, legs, e.graphStore(), now.Add(2*wispGCRefusalRecheck))
	})
	if !strings.Contains(first, "not purging closed root "+root.ID) {
		t.Fatalf("first refusal not reported:\n%s", first)
	}
	if strings.Contains(again, "not purging closed root "+root.ID) {
		t.Fatalf("the recheck reprinted an unchanged refusal:\n%s", again)
	}
}

// Nit: refusals of roots that disappear are pruned.
func TestWorkArmPrunesRefusalsOfGoneRoots(t *testing.T) {
	store := newBatchDeleteWorkStore()
	root := closedPoured(t, store, "held")
	kids, _ := store.Children(root.ID, beads.IncludeClosed)
	inProgress := "in_progress"
	if err := store.Update(kids[0].ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	enforcedLane(t, func() {
		runLane(t, gc, legs, e.graphStore(), now)
		if len(gc.notes.refused) == 0 {
			t.Fatal("refusal not remembered")
		}
		for _, id := range []string{kids[0].ID, root.ID} {
			if err := store.Delete(id); err != nil {
				t.Fatal(err)
			}
		}
		runLane(t, gc, legs, e.graphStore(), now)
	})
	if len(gc.notes.refused) != 0 {
		t.Fatalf("refusals of deleted roots kept: %v", gc.notes.refused)
	}
}

// Must-fix 3 own_ignores_own / should-fix 5 own_ignores_graph: the work arm's
// holder check reads the root's own leg AND the graph store.
func TestWorkArmHolderCheckReadsItsOwnLegAndTheGraph(t *testing.T) {
	for _, where := range []string{"own-leg", "graph"} {
		t.Run(where, func(t *testing.T) {
			store := newBatchDeleteWorkStore()
			root := stepless(t, store, beads.Bead{Title: "attached v1 molecule", Type: "molecule"})
			e, legs := laneLegs(t, store)
			holderStore := beads.Store(store)
			if where == "graph" {
				holderStore = e.graphStore()
			}
			stepless(t, holderStore, beads.Bead{Title: "live holder", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}})
			enforcedLane(t, func() { runLane(t, newWorkMoleculeGC(), legs, e.graphStore(), time.Now().Add(30*time.Minute)) })
			if laneGet(t, store, root.ID).Status != "open" {
				t.Fatalf("stepless root %s closed while a live holder in the %s points at it", root.ID, where)
			}
		})
	}
}

// laneGetFailingStore fails reads of one id.
type laneGetFailingStore struct {
	beads.Store
	failID string
}

func (s laneGetFailingStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("store unavailable")
	}
	return s.Store.Get(id)
}

// Must-fix 3 src_dead_shortcircuit and src_err_dead: a dead recorded source
// does not end the check (another live holder still holds the root), and a
// source that cannot be read counts as live.
func TestHolderGuardRecordedSourceEdges(t *testing.T) {
	t.Run("dead-source-but-live-holder", func(t *testing.T) {
		e, _, legs := twoRigSplitEnv(t)
		graph := e.graphStore()
		dead := stepless(t, e.rig, beads.Bead{Title: "done source", Type: "task"})
		if err := e.rig.Close(dead.ID); err != nil {
			t.Fatal(err)
		}
		root := stepless(t, graph, beads.Bead{Title: "attached wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp, beadmeta.SourceBeadIDMetadataKey: dead.ID, beadmeta.SourceStoreRefMetadataKey: "rig:" + e.rigName,
		}})
		stepless(t, e.rig, beads.Bead{Title: "re-attached live holder", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}})
		withCloseAbandonedEnforced(t, func() { runGCWithLegs(t, legs, graph, time.Now().Add(30*time.Minute)) })
		wantStatus(t, graph, root.ID, "open", "a live holder still points at it")
	})
	t.Run("unreadable-source", func(t *testing.T) {
		e := newSplitEnvWithRig(t, true)
		src := stepless(t, e.rig, beads.Bead{Title: "source", Type: "task"})
		legs := workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), map[string]beads.Store{e.rigName: laneGetFailingStore{Store: e.rig, failID: src.ID}})
		graph := e.graphStore()
		root := stepless(t, graph, beads.Bead{Title: "attached wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp, beadmeta.SourceBeadIDMetadataKey: src.ID, beadmeta.SourceStoreRefMetadataKey: "rig:" + e.rigName,
		}})
		withCloseAbandonedEnforced(t, func() { runGCWithLegs(t, legs, graph, time.Now().Add(30*time.Minute)) })
		wantStatus(t, graph, root.ID, "open", "its source could not be read")
	})
}

// Should-fix 4: a pointer whose store ref names no store this city serves is
// no pointer: the root takes the legacy path, and a suspended rig (which may
// hold the live source) keeps a molecule-typed root open.
func TestHolderGuardUnusableSourceRefIsNoPointer(t *testing.T) {
	for _, ref := range []string{"", "city:", "rig:renamed-rig", "class:infra", "city:another-city"} {
		t.Run(fmt.Sprintf("ref=%q", ref), func(t *testing.T) {
			e, suspended, legs := twoRigSplitEnv(t)
			graph := e.graphStore()
			src := stepless(t, suspended, beads.Bead{Title: "live source in the suspended rig", Type: "task"})
			md := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp, beadmeta.SourceBeadIDMetadataKey: src.ID}
			if ref != "" {
				md[beadmeta.SourceStoreRefMetadataKey] = ref
			}
			root := stepless(t, graph, beads.Bead{Title: "attached wisp", Type: "molecule", Metadata: md})
			withCloseAbandonedEnforced(t, func() { runGCWithLegs(t, legs, graph, time.Now().Add(30*time.Minute)) })
			wantStatus(t, graph, root.ID, "open", "an unusable ref proves nothing and rig-B is suspended")
		})
	}
}

// gate_fail_cache_off: a formula that fails to compile for the order gate is
// not recompiled on the next ask within the TTL.
func TestOrderGateRemembersACompileFailure(t *testing.T) {
	prev := orderGateCompile
	compiles := 0
	orderGateCompile = func(context.Context, string, []string, map[string]string) (*formula.Recipe, error) {
		compiles++
		return nil, errors.New("broken formula")
	}
	t.Cleanup(func() { orderGateCompile = prev })
	e := newSplitEnv(t, true)
	a := pouredOrderFor(t)
	m, _ := newWorkClassOrderDispatcher(t, e, a, nil)
	m.orderPoursWork(context.Background(), a)
	m.orderPoursWork(context.Background(), a)
	if compiles != 1 {
		t.Fatalf("compiled %d time(s), want 1: the failure must be remembered for a while", compiles)
	}
}

// close_memo_off: once busy molecules are found not closable, the next pass
// does not walk them again (their outcome is remembered), whatever the cursor.
func TestWorkArmRemembersNotClosableRoots(t *testing.T) {
	store := newBatchDeleteWorkStore()
	for i := 0; i < 3; i++ {
		busy := stepless(t, store, beads.Bead{Title: fmt.Sprintf("busy-%d", i), Type: "molecule"})
		stepless(t, store, beads.Bead{Title: "live step", Type: "step", ParentID: busy.ID})
	}
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(30 * time.Minute)
	var first, second string
	enforcedLane(t, func() {
		first = runLane(t, gc, legs, e.graphStore(), now)
		second = runLane(t, gc, legs, e.graphStore(), now)
	})
	if !strings.Contains(first, "walked 3 root(s)") {
		t.Fatalf("first pass did not walk the 3 busy roots:\n%s", first)
	}
	if strings.Contains(second, "walked") {
		t.Fatalf("second pass re-walked roots already found not closable:\n%s", second)
	}
}

// fail_memo_off: a root whose delete failed is not attempted again on the
// next pass.
func TestWorkArmDoesNotRetryAFailedDeleteEveryPass(t *testing.T) {
	var asked []string
	inner := newBatchDeleteWorkStore()
	store := laneFailingDeleteStore{batchDeleteWorkStore: inner, asked: &asked}
	closedPoured(t, inner, "a")
	e, legs := laneLegs(t, store)
	gc := newWorkMoleculeGC()
	now := time.Now().Add(2 * time.Hour)
	enforcedLane(t, func() {
		runLane(t, gc, legs, e.graphStore(), now)
		runLane(t, gc, legs, e.graphStore(), now)
	})
	if len(asked) != 2 { // root + step, one attempt
		t.Fatalf("delete attempts %v over two passes, want one attempt", asked)
	}
}
