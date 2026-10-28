package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The wisp GC on a split city (R7 and the work-class molecule roots).
//
// The GC runs over the graph-class store. Two of its reads belong to the work
// side on a split city:
//
//   - the abandoned-root sweep's live-holder guard: the holders of an attached
//     root are WORK beads carrying molecule_id / workflow_id, so listing them
//     in the binding always answered "no live holder" and an attached
//     root-only wisp became closable one TTL after pour while its source was
//     live (R7);
//   - work-class molecule roots: a v1 poured molecule is work class and lives
//     on the work ledger, so a GC that enumerated roots in the binding alone
//     never closed an abandoned one nor collected a closed one.
//
// Both topologies; the split leg's binding is a real SQLite store.

func wispGCLegsFor(e splitEnv) wispGCLegs {
	if !e.split {
		return nil
	}
	return workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), nil)
}

// withWorkMoleculesEnforced runs fn with the work arm allowed to mutate.
func withWorkMoleculesEnforced(t *testing.T, fn func()) {
	t.Helper()
	prev := workMoleculesEnforced
	workMoleculesEnforced = func() bool { return true }
	defer func() { workMoleculesEnforced = prev }()
	fn()
}

func runSplitWispGC(t *testing.T, e splitEnv, now time.Time) {
	t.Helper()
	withWorkMoleculesEnforced(t, func() { runSplitWispGCDefault(t, e, now) })
}

// runSplitWispGCDefault runs the GC with the abandoned-root closer enforced
// and the work arm at its default (dry-run unless the test enforced it).
func runSplitWispGCDefault(t *testing.T, e splitEnv, now time.Time) {
	t.Helper()
	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newTestGC()
			if err := wg.pass(wispGCLegsFor(e), e.graphStore(), now); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})
}

// R7: a live work source keeps its attached root-only wisp open.
func TestWispGCAbandonedRootHolderGuardSeesAWorkSource(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		graph := e.graphStore()
		root, err := graph.Create(beads.Bead{Title: "attached root-only wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.work.Create(beads.Bead{Title: "live source", Type: "task", Metadata: map[string]string{
			beadmeta.MoleculeIDMetadataKey: root.ID,
		}}); err != nil {
			t.Fatal(err)
		}
		runSplitWispGC(t, e, time.Now().Add(30*time.Minute))
		got, err := graph.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "open" {
			t.Fatalf("attached wisp %s status=%q with a live work source, want open", root.ID, got.Status)
		}
	})
}

// The far side: once the work source is terminal the stepless root reaps.
func TestWispGCAbandonedRootClosesWhenItsWorkSourceIsTerminal(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		graph := e.graphStore()
		root, err := graph.Create(beads.Bead{Title: "attached root-only wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		src, err := e.work.Create(beads.Bead{Title: "done source", Type: "task", Metadata: map[string]string{
			beadmeta.MoleculeIDMetadataKey: root.ID,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.work.Close(src.ID); err != nil {
			t.Fatal(err)
		}
		runSplitWispGC(t, e, time.Now().Add(30*time.Minute))
		got, err := graph.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("attached wisp %s status=%q with a terminal source, want closed", root.ID, got.Status)
		}
	})
}

// A v1 poured molecule on the work ledger whose steps are all terminal is an
// abandoned root the sweep closes.
func TestWispGCClosesAnAbandonedWorkClassMoleculeRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		root, steps, owner := pourV1Molecule(t, e, "")
		if owner != e.work {
			t.Fatalf("v1 poured molecule went to %T, want the work store", owner)
		}
		for _, s := range steps {
			if err := e.work.Close(s.ID); err != nil {
				t.Fatal(err)
			}
		}
		runSplitWispGC(t, e, time.Now().Add(30*time.Minute))
		got, err := e.work.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("abandoned work-class molecule root %s status=%q, want closed", root.ID, got.Status)
		}
	})
}

// A closed v1 poured molecule on the work ledger past the TTL is collected.
//
// The purge deletes through the store's orphan-preserving batch delete, which
// the production work stores (bd, Dolt) have and the kit's strict in-memory
// leaf does not, so this row reads its work leg from a store that has it.
func TestWispGCPurgesAnExpiredWorkClassMoleculeRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		work := newBatchDeleteWorkStore()
		graph := e.graphStore()
		var legs wispGCLegs
		if e.split {
			legs = workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: work}), nil)
		} else {
			graph = work
		}
		root, err := work.Create(beads.Bead{Title: "v1 poured", Type: "molecule"})
		if err != nil {
			t.Fatal(err)
		}
		step, err := work.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{step.ID, root.ID} {
			if err := work.Close(id); err != nil {
				t.Fatal(err)
			}
		}
		wg := newTestGC()
		withWorkMoleculesEnforced(t, func() {
			if err := wg.pass(legs, graph, time.Now().Add(2*time.Hour)); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
		if _, err := work.Get(root.ID); err == nil {
			t.Fatalf("closed work-class molecule root %s survived a GC past its TTL", root.ID)
		}
	})
}

// batchDeleteWorkStore is a work ledger with the orphan-preserving batch delete
// the production work stores have.
type batchDeleteWorkStore struct{ *beads.MemStore }

func newBatchDeleteWorkStore() batchDeleteWorkStore {
	return batchDeleteWorkStore{MemStore: beads.NewMemStore()}
}

func (s batchDeleteWorkStore) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

// The work arm never touches a plain work bead.
func TestWispGCLeavesPlainWorkAlone(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		task, err := e.work.Create(beads.Bead{Title: "plain task", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.work.Close(task.ID); err != nil {
			t.Fatal(err)
		}
		runSplitWispGC(t, e, time.Now().Add(2*time.Hour))
		if _, err := e.work.Get(task.ID); err != nil {
			t.Fatalf("plain closed task %s was collected by the wisp GC: %v", task.ID, err)
		}
	})
}

// purgeFixture builds a closed v1 poured molecule on a work ledger that can
// batch-delete, and the legs and graph store a GC pass over it uses.
func purgeFixture(t *testing.T, e splitEnv) (work batchDeleteWorkStore, graph beads.Store, legs wispGCLegs) {
	t.Helper()
	work = newBatchDeleteWorkStore()
	graph = e.graphStore()
	if e.split {
		legs = workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: work}), nil)
	} else {
		graph = work
	}
	return work, graph, legs
}

// Must-fix: a closed root is not proof its closure is dead. A step reopened
// after its molecule closed, linked only by ParentID (no gc.root_bead_id, as v1
// poured steps are), must stop the purge.
func TestWispGCPurgeRefusesAClosureWithALiveStep(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		work, graph, legs := purgeFixture(t, e)
		root, err := work.Create(beads.Bead{Title: "v1 poured", Type: "molecule"})
		if err != nil {
			t.Fatal(err)
		}
		step, err := work.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{step.ID, root.ID} {
			if err := work.Close(id); err != nil {
				t.Fatal(err)
			}
		}
		inProgress := "in_progress"
		if err := work.Update(step.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
		wg := newTestGC()
		withWorkMoleculesEnforced(t, func() {
			if err := wg.pass(legs, graph, time.Now().Add(2*time.Hour)); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
		got, err := work.Get(step.ID)
		if err != nil {
			t.Fatalf("in-progress step %s under closed root %s was purged: %v", step.ID, root.ID, err)
		}
		if got.Status != "in_progress" {
			t.Fatalf("step %s status=%q, want in_progress", step.ID, got.Status)
		}
		if _, err := work.Get(root.ID); err != nil {
			t.Fatalf("root %s was purged while its closure held a live step: %v", root.ID, err)
		}
	})
}

// The work arm is dry-run by default: it logs, and closes or purges nothing.
func TestWispGCWorkArmIsDryRunByDefault(t *testing.T) {
	e := newSplitEnv(t, true)
	work, graph, legs := purgeFixture(t, e)
	expired, err := work.Create(beads.Bead{Title: "expired v1 poured", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Close(expired.ID); err != nil {
		t.Fatal(err)
	}
	abandoned, err := work.Create(beads.Bead{Title: "abandoned v1 poured", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	step, err := work.Create(beads.Bead{Title: "step", Type: "step", ParentID: abandoned.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newTestGC()
			if err := wg.pass(legs, graph, time.Now().Add(2*time.Hour)); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})
	if _, err := work.Get(expired.ID); err != nil {
		t.Fatalf("dry-run work arm purged %s: %v", expired.ID, err)
	}
	if got, _ := work.Get(abandoned.ID); got.Status == "closed" {
		t.Fatalf("dry-run work arm closed %s", abandoned.ID)
	}
}

// bd proto templates are molecule rows; the work arm never touches one.
func TestWispGCWorkArmSkipsProtoTemplates(t *testing.T) {
	e := newSplitEnv(t, true)
	work, graph, legs := purgeFixture(t, e)
	proto, err := work.Create(beads.Bead{Title: "mol-proto", Type: "molecule", Labels: []string{bdTemplateLabel}})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Close(proto.ID); err != nil {
		t.Fatal(err)
	}
	wg := newTestGC()
	withWorkMoleculesEnforced(t, func() {
		if err := wg.pass(legs, graph, time.Now().Add(2*time.Hour)); err != nil {
			t.Fatalf("runGC: %v", err)
		}
	})
	if _, err := work.Get(proto.ID); err != nil {
		t.Fatalf("proto template %s was purged: %v", proto.ID, err)
	}
}

// A suspended rig is not read, yet it can hold the live source of an attached
// root-only wisp: the holder guard leaves the root open while it is suspended.
func TestWispGCHolderGuardLeavesARootOpenWhileARigIsSuspended(t *testing.T) {
	e := newSplitEnvWithRig(t, true)
	graph := e.graphStore()
	root, err := graph.Create(beads.Bead{Title: "attached root-only wisp", Type: "molecule", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWisp,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.rig.Create(beads.Bead{Title: "live source in a suspended rig", Type: "task", Metadata: map[string]string{
		beadmeta.MoleculeIDMetadataKey: root.ID,
	}}); err != nil {
		t.Fatal(err)
	}
	e.cfg.Rigs[0].Suspended = true
	legs := workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), e.rigStores)
	if len(legs.suspendedRigs()) != 1 {
		t.Fatalf("suspendedRigs = %v, want the one suspended rig", legs.suspendedRigs())
	}
	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newTestGC()
			if err := wg.pass(legs, graph, time.Now().Add(30*time.Minute)); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})
	if got, _ := graph.Get(root.ID); got.Status != "open" {
		t.Fatalf("attached wisp %s status=%q while its source's rig is suspended, want open", root.ID, got.Status)
	}
}

// unreadableLegs is a leg set that cannot be read (a refused city).
type unreadableLegs struct{}

func (unreadableLegs) each(func(beads.Store))            {}
func (unreadableLegs) unusable() error                   { return errNoWorkStore }
func (unreadableLegs) suspendedRigs() []string           { return nil }
func (unreadableLegs) eachLeg(func(string, beads.Store)) {}
func (unreadableLegs) storeForRef(string) (beads.Store, bool, bool) {
	return nil, false, false
}

// Should-fix 4: a leg set that cannot be read leaves a stepless root open.
func TestWispGCHolderGuardFailsClosedOnUnreadableLegs(t *testing.T) {
	e := newSplitEnv(t, true)
	graph := e.graphStore()
	root, err := graph.Create(beads.Bead{Title: "root-only wisp", Type: "molecule", Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWisp,
	}})
	if err != nil {
		t.Fatal(err)
	}
	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			wg := newTestGC()
			if err := wg.pass(unreadableLegs{}, graph, time.Now().Add(30*time.Minute)); err != nil {
				t.Fatalf("runGC: %v", err)
			}
		})
	})
	if got, _ := graph.Get(root.ID); got.Status != "open" {
		t.Fatalf("stepless root %s status=%q with unreadable legs, want open", root.ID, got.Status)
	}
}

// The work arm's dry run over a rig leg shaped like maintainer-city's gascity
// rig only lists and counts: one summary line, nothing mutated, no closure
// walked. Enforcing, it closes the abandoned roots, purges the expired
// closures, refuses the one holding a reopened step, and skips the template.
func TestWispGCWorkArmDryRunOnARigShapedLikeMaintainerCity(t *testing.T) {
	e := newSplitEnvWithRig(t, true)
	rig := newBatchDeleteWorkStore()
	mcShapedRigFixture(t, rig)
	before := len(allBeads(t, rig))
	prevBudget := wispGCWorkArmWalkBudget
	wispGCWorkArmWalkBudget = 100
	t.Cleanup(func() { wispGCWorkArmWalkBudget = prevBudget })

	var logs bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	legs := workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), map[string]beads.Store{e.rigName: rig})
	wg := newTestGC()
	runPass := func() {
		withCloseAbandonedEnforced(t, func() {
			withCloseAbandonedTTL(t, 5*time.Minute, func() {
				if err := wg.pass(legs, e.graphStore(), time.Now().Add(48*time.Hour)); err != nil {
					t.Logf("runGC: %v", err)
				}
			})
		})
	}
	runPass()
	t.Logf("work-arm dry run:\n%s", logs.String())
	if after := len(allBeads(t, rig)); after != before {
		t.Fatalf("dry run changed the rig: %d beads before, %d after", before, after)
	}
	if want := "2 open work-class molecule root(s) idle past 5m0s, 27 closed root(s) past retention"; !strings.Contains(logs.String(), want) {
		t.Errorf("dry-run log lacks %q", want)
	}

	logs.Reset()
	withWorkMoleculesEnforced(t, runPass)
	t.Logf("work-arm enforced pass:\n%s", logs.String())
	out := logs.String()
	for _, want := range []string{"closed abandoned root", "not purging closed root", "holds a bead that is not closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("enforced log lacks %q", want)
		}
	}

	// The refusal is remembered: the next pass neither walks nor re-logs it.
	logs.Reset()
	withWorkMoleculesEnforced(t, runPass)
	again := logs.String()
	t.Logf("work-arm second enforced pass:\n%s", again)
	if strings.Contains(again, "not purging closed root") {
		t.Errorf("the refused closure was re-walked or re-logged on the next pass")
	}
	if !strings.Contains(again, "(0 walked this pass, 1 remembered)") {
		t.Errorf("second pass summary does not report the remembered refusal")
	}
}

// testGC runs both wisp GC arms the way production does — the graph arm on
// the tick (runGC) and the work arm on its lane (workMoleculeGC.pass) — with
// state that persists across passes, as both trackers' does.
type testGC struct {
	wg   wispGC
	work *workMoleculeGC
}

func newTestGC() *testGC {
	return &testGC{wg: newWispGC(5*time.Minute, time.Hour, 0), work: newWorkMoleculeGC()}
}

func (g *testGC) pass(legs wispGCLegs, graph beads.Store, now time.Time) error {
	_, err := g.wg.runGC(legs, beads.GraphStore{Store: graph}, beads.SessionStore{}, beads.MailStore{}, now)
	_, werr := g.work.pass(context.Background(), legs, graph, time.Hour, now)
	return errors.Join(err, werr)
}

// withWorkArmBudget runs fn with the work arm's per-pass walk budget set to n.
func withWorkArmBudget(t *testing.T, n int, fn func()) {
	t.Helper()
	prev := wispGCWorkArmWalkBudget
	wispGCWorkArmWalkBudget = n
	defer func() { wispGCWorkArmWalkBudget = prev }()
	fn()
}

// Nit: on a single-store city, a stepless root's source pointer resolves only
// for THIS city's ref, as storeForRef does on a split city.
func TestSingleStoreSourceRefResolution(t *testing.T) {
	graph := beads.NewMemStore()
	h := wispGCHolders{graph: graph, cityName: "home"}
	for ref, want := range map[string]bool{
		"city:home":   true,
		"city:other":  false,
		"class:graph": false,
		"rig:r1":      false,
		"":            false,
	} {
		root := beads.Bead{Metadata: map[string]string{beadmeta.SourceStoreRefMetadataKey: ref}}
		store, _, ok := h.sourceStore(root)
		if ok != want || (ok && store != beads.Store(graph)) {
			t.Errorf("ref %q: ok=%v; want %v", ref, ok, want)
		}
	}
}
