package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Typed-stores T7g part 2: the wisp GC reads through the epoch's
// Molecule{Roots} plan over the census legs (t7g-design.md §4, owner decision
// 3). The GC's leg set is safety-critical, so the table here proves the
// conversion changed how the legs are read and nothing else: the pre-T7g
// leaf reads are frozen below and compared read for read, and the whole GC
// (graph arm, abandoned-root closer, holder guard, work arm, purges) is run
// over two copies of each shape, one through the pre-T7g reads and one
// through the production readers (WorkLegs.wispGCRoots, wispGCGraphRoots),
// and must leave the same rows behind: the purge set is identical.

// --- frozen references: the pre-T7g leaf reads, over raw stores ---

func enumerateWispGCRootsReference(store beads.Store, selectors []wispGCRootSelector, statuses ...string) ([]beads.Bead, error) {
	entries := make([]beads.Bead, 0)
	seen := map[string]struct{}{}
	for _, status := range statuses {
		for _, sel := range selectors {
			q := sel.query
			q.Status = status
			items, err := store.List(q)
			if err != nil {
				return nil, fmt.Errorf("listing %s %s roots: %w", status, sel.label, err)
			}
			for _, item := range items {
				if item.ID == "" {
					continue
				}
				if _, ok := seen[item.ID]; ok {
					continue
				}
				seen[item.ID] = struct{}{}
				entries = append(entries, item)
			}
		}
	}
	return entries, nil
}

func closedWispGCEntriesReference(store beads.Store) (roots []beads.Bead, err error) {
	err = beads.FederatedRead(func() (err error) {
		roots, err = enumerateWispGCRootsReference(store, wispGCRootSelectors(), "closed")
		return err
	})
	return roots, err
}

func openWispGCRootCandidatesReference(store beads.Store) (roots []beads.Bead, err error) {
	err = beads.FederatedRead(func() (err error) {
		roots, err = enumerateWispGCRootsReference(store, wispGCRootSelectors(), "open", "in_progress")
		return err
	})
	return roots, err
}

func enumerateWorkClassMoleculeRootsReference(store beads.Store, statuses ...string) ([]beads.Bead, error) {
	sel := wispGCMoleculeRootSelector()
	sel.query.TierMode = beads.TierIssues
	var roots []beads.Bead
	err := beads.FederatedRead(func() (err error) {
		roots, err = enumerateWispGCRootsReference(store, []wispGCRootSelector{sel}, statuses...)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := roots[:0]
	for _, root := range roots {
		if !beads.IsMoleculeType(root.Type) || strings.TrimSpace(root.ParentID) != "" || coordclass.Classify(root) != coordclass.ClassWork {
			continue
		}
		if gcRefHasLabel(root.Labels, bdTemplateLabel) {
			continue
		}
		out = append(out, root)
	}
	return out, nil
}

func storeHoldsLiveAttachmentSourceReference(store beads.Store, rootID string) bool {
	for _, key := range []string{beadmeta.MoleculeIDMetadataKey, "workflow_id"} {
		var holders []beads.Bead
		err := beads.FederatedRead(func() (err error) {
			holders, err = store.List(beads.ListQuery{Metadata: map[string]string{key: rootID}, IncludeClosed: true, TierMode: beads.TierBoth})
			return err
		})
		if err != nil {
			return true
		}
		for _, h := range holders {
			if h.ID != rootID && !convoycore.IsTerminalStatus(h.Status) {
				return true
			}
		}
	}
	return false
}

// referenceHolderWalk is the pre-T7g holder walk's leg reads (readHolders):
// the graph store, then each leg but graph.
func referenceHolderWalk(graph beads.Store, legs wispGCLegs, rootID string) bool {
	if storeHoldsLiveAttachmentSourceReference(graph, rootID) {
		return true
	}
	live := false
	if legs != nil {
		legs.each(func(store beads.Store) {
			if live || store == nil || sameBeadStore(store, graph) {
				return
			}
			live = storeHoldsLiveAttachmentSourceReference(store, rootID)
		})
	}
	return live
}

// --- the GC shape corpus ---

// gcShape is one GC shape: the graph store, the census legs (nil: the routes
// relocate nothing, so the GC reads graph alone), the epoch, and every store
// a row can sit in, named.
type gcShape struct {
	graph  beads.Store
	legs   wispGCLegs
	epoch  storeref.Stores
	stores map[string]beads.Store
}

// roots are the production readers for the shape, as tickWispGC and the
// work lane build them.
func (s gcShape) roots() wispGCRoots {
	if s.legs == nil {
		return wispGCGraphRoots(s.epoch, s.graph)
	}
	return s.legs.wispGCRoots(s.epoch, s.graph)
}

// gcRecipes are the shapes: unsplit (graph is the city store, and the rigs
// hold v1 molecule roots the GC must never read), unsplit with the gate's
// views, whole split with rigs (the census legs), a suspended rig the census
// leaves out, a stranded split served degraded (a graph-class wisp root on
// the work ledger), and a refused binding.
func gcRecipes(t *testing.T) map[string]func() gcShape {
	census := func(city beads.Store, rigs map[string]beads.Store, suspended string) wispGCLegs {
		dir := t.TempDir()
		cfg := &config.City{Workspace: config.Workspace{Name: "c"}}
		for _, name := range []string{"alpha", "bravo"} {
			cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: filepath.Join(dir, name), Suspended: name == suspended})
		}
		return workLegsFromCensus(dir, cfg, cityWorkLegOf(beads.WorkStore{Store: city}), rigs)
	}
	rigsOf := func() (map[string]beads.Store, map[string]testRig) {
		alpha, bravo := newTestMem("ra"), newTestMem("rb")
		return map[string]beads.Store{"alpha": alpha, "bravo": bravo},
			map[string]testRig{"alpha": {store: alpha, prefix: "ra"}, "bravo": {store: bravo, prefix: "rb"}}
	}
	split := func(suspended string, graph beads.Store) gcShape {
		city := newTestMem("ga")
		rigs, testRigs := rigsOf()
		stores := map[string]beads.Store{"city": city, "graph": graph, "alpha": rigs["alpha"], "bravo": rigs["bravo"]}
		return gcShape{graph: graph, legs: census(city, rigs, suspended), epoch: testStoresWithRigs(city, graph, testRigs), stores: stores}
	}
	return map[string]func() gcShape{
		"unsplit": func() gcShape {
			city := newTestMem("ga")
			rigs, testRigs := rigsOf()
			return gcShape{graph: city, epoch: testStoresWithRigs(city, city, testRigs), stores: map[string]beads.Store{"city": city, "alpha": rigs["alpha"], "bravo": rigs["bravo"]}}
		},
		"gate on": func() gcShape {
			physical := newTestMem("ga")
			epoch := storeref.NewStores(storeref.Topology{Work: storeref.Leg{Ref: storeref.WorkRef, Store: physical}}, storeref.StoresOptions{Projection: splitStrictGate{}})
			return gcShape{graph: epoch.Graph().Store, epoch: epoch, stores: map[string]beads.Store{"city": physical}}
		},
		"whole split":               func() gcShape { return split("", newTestMem("gcg")) },
		"suspended rig":             func() gcShape { return split("bravo", newTestMem("gcg")) },
		"stranded, served degraded": func() gcShape { return split("", newTestMem("gcg")) },
		"refused": func() gcShape {
			return split("", refusedClassStore{err: standingStorageRefusal{err: errors.New("storage refused: run `gc storage migrate`")}})
		},
	}
}

// seedGC plants the GC's whole decision surface in s: an expired closed wisp
// root and an abandoned open stepless one held by a live source in rig alpha
// in graph, an open stepless one whose source is terminal, a v1 molecule root
// closed and expired and one open with every step closed on each work store,
// and, on the stranded shape, a graph-class wisp root on the work ledger.
func seedGC(t *testing.T, name string, s gcShape) {
	t.Helper()
	graph := s.graph
	if _, err := graph.Get("gcg-probe"); !storeref.IsStandingRefusal(err) {
		fedCreate(t, graph, beads.Bead{ID: "gcg-dead", Title: "closed wisp", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
		if err := graph.Close("gcg-dead"); err != nil {
			t.Fatal(err)
		}
		fedCreate(t, graph, beads.Bead{ID: "gcg-held", Title: "held wisp", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
		fedCreate(t, graph, beads.Bead{ID: "gcg-free", Title: "free wisp", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	}
	holderStore := s.stores["alpha"]
	if holderStore == nil || s.legs == nil {
		holderStore = s.stores["city"]
	}
	fedCreate(t, holderStore, beads.Bead{ID: gcIDIn(holderStore, "src-live"), Title: "live source", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: "gcg-held"}})
	src := fedCreate(t, holderStore, beads.Bead{ID: gcIDIn(holderStore, "src-done"), Title: "done source", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: "gcg-free"}})
	if err := holderStore.Close(src.ID); err != nil {
		t.Fatal(err)
	}
	for _, key := range gcStoreKeys(s.stores) {
		store := s.stores[key]
		if key == "graph" || store == nil {
			continue
		}
		dead := fedCreate(t, store, beads.Bead{ID: gcIDIn(store, "mol-dead"), Title: "closed v1 molecule", Type: "molecule"})
		if err := store.Close(dead.ID); err != nil {
			t.Fatal(err)
		}
		open := fedCreate(t, store, beads.Bead{ID: gcIDIn(store, "mol-open"), Title: "abandoned v1 molecule", Type: "molecule"})
		step := fedCreate(t, store, beads.Bead{ID: gcIDIn(store, "mol-step"), Title: "step", Type: "step", ParentID: open.ID})
		if err := store.Close(step.ID); err != nil {
			t.Fatal(err)
		}
	}
	if name == "stranded, served degraded" {
		fedCreate(t, s.stores["city"], beads.Bead{ID: "ga-stranded", Title: "stranded wisp root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	}
}

func gcIDIn(store beads.Store, suffix string) string {
	if m, ok := store.(*beads.MemStore); ok && m.IDPrefix != "" {
		return m.IDPrefix + "-" + suffix
	}
	return "ga-" + suffix
}

func gcStoreKeys(m map[string]beads.Store) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func gcRows(t *testing.T, s gcShape) string {
	t.Helper()
	var out []string
	for _, key := range gcStoreKeys(s.stores) {
		store := s.stores[key]
		if store == nil || (key == "graph" && sameBeadStore(store, s.stores["city"])) {
			continue
		}
		rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			out = append(out, key+": "+err.Error())
			continue
		}
		for _, b := range rows {
			out = append(out, key+"/"+b.ID+"="+b.Status)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func gcIDs(bs []beads.Bead) string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.ID)
	}
	return strings.Join(out, ",")
}

// TestWispGCReadsAgreeWithTheFrozenReference compares the GC's reads, one
// for one, over each shape: the graph arm's closed and open root listings,
// the work arm's leg set and each leg's listings, and the holder answer for
// every candidate root.
func TestWispGCReadsAgreeWithTheFrozenReference(t *testing.T) {
	for name, build := range gcRecipes(t) {
		t.Run(name, func(t *testing.T) {
			s := build()
			seedGC(t, name, s)
			roots := s.roots()
			graph, err := roots.graphReader()
			if err != nil {
				t.Fatalf("no graph reader: %v", err)
			}
			for _, tc := range []struct {
				name string
				ref  func() ([]beads.Bead, error)
				got  func() ([]beads.Bead, error)
			}{
				{"closed roots", func() ([]beads.Bead, error) { return closedWispGCEntriesReference(s.graph) }, func() ([]beads.Bead, error) { return closedWispGCEntries(graph) }},
				{"open roots", func() ([]beads.Bead, error) { return openWispGCRootCandidatesReference(s.graph) }, func() ([]beads.Bead, error) { return openWispGCRootCandidates(graph) }},
			} {
				want, werr := tc.ref()
				got, gerr := tc.got()
				if gcIDs(got) != gcIDs(want) || fmt.Sprint(gerr) != fmt.Sprint(werr) {
					t.Errorf("%s = %s, %v; reference %s, %v", tc.name, gcIDs(got), gerr, gcIDs(want), werr)
				}
			}
			// The work arm's legs: the reference reads each census leg but
			// graph; the converted pass reads each through its ref's reader.
			var refLegs, gotLegs []string
			if s.legs != nil {
				s.legs.eachLeg(func(ref string, store beads.Store) {
					if store == nil || sameBeadStore(store, s.graph) {
						return
					}
					refLegs = append(refLegs, ref)
					leg, err := roots.workLeg(ref)
					if err != nil {
						t.Errorf("work leg %q: %v", ref, err)
						return
					}
					gotLegs = append(gotLegs, ref)
					if !beads.SameStore(leg, store) {
						t.Errorf("work leg %q reads another store than the census's", ref)
					}
					for _, statuses := range [][]string{{"open", "in_progress"}, {"closed"}} {
						want, werr := enumerateWorkClassMoleculeRootsReference(store, statuses...)
						got, gerr := enumerateWorkClassMoleculeRoots(leg, statuses...)
						if gcIDs(got) != gcIDs(want) || fmt.Sprint(gerr) != fmt.Sprint(werr) {
							t.Errorf("leg %q %v roots = %s, reference %s", ref, statuses, gcIDs(got), gcIDs(want))
						}
					}
				})
			}
			if fmt.Sprint(gotLegs) != fmt.Sprint(refLegs) {
				t.Errorf("work arm legs = %v, reference %v", gotLegs, refLegs)
			}
			if s.legs == nil && len(roots.work) != 0 {
				t.Errorf("a city whose routes relocate nothing has work-leg readers %v: the GC must read graph alone", roots.work)
			}
			// The holder answer for every candidate root.
			candidates, _ := openWispGCRootCandidatesReference(s.graph)
			h := wispGCHolders{graph: s.graph, roots: roots, legs: s.legs}
			for _, root := range candidates {
				if got, want := h.readHolders(root, false, nil, ""), referenceHolderWalk(s.graph, s.legs, root.ID); got != want {
					t.Errorf("holders of %s = %v, reference %v", root.ID, got, want)
				}
			}
		})
	}
}

// TestWispGCPurgeSetIsUnchanged runs the whole GC, every arm enforced, over
// two copies of each shape, once through the pre-T7g reads (each leg read
// as its own store: testWispGCRoots) and once through the production
// readers, and requires the same rows to survive in the same states.
func TestWispGCPurgeSetIsUnchanged(t *testing.T) {
	run := func(s gcShape, roots wispGCRoots) {
		withWorkMoleculesEnforced(t, func() {
			withCloseAbandonedEnforced(t, func() {
				withCloseAbandonedTTL(t, 5*time.Minute, func() {
					wg := newTestGC()
					now := time.Now().Add(48 * time.Hour)
					_, err := wg.wg.runGC(roots, s.legs, beads.GraphStore{Store: s.graph}, beads.SessionStore{}, beads.MailStore{}, now)
					_, werr := wg.work.pass(context.Background(), roots, s.legs, s.graph, time.Hour, now)
					if e := errors.Join(err, werr); e != nil {
						t.Logf("GC pass: %v", e)
					}
				})
			})
		})
	}
	for name, build := range gcRecipes(t) {
		t.Run(name, func(t *testing.T) {
			ref, conv := build(), build()
			seedGC(t, name, ref)
			seedGC(t, name, conv)
			seeded := gcRows(t, ref)
			run(ref, testWispGCRoots(ref.graph, ref.legs))
			run(conv, conv.roots())
			if got, want := gcRows(t, conv), gcRows(t, ref); got != want {
				t.Errorf("rows after the GC:\n got  %s\n want %s", got, want)
			}
			if name == "whole split" && gcRows(t, ref) == seeded {
				t.Error("the reference GC changed nothing on the whole split: the comparison is vacuous")
			}
		})
	}
}

// TestWispGCNeverReadsARigOnAnUnsplitCity pins the unsplit case: with no
// census legs (the routes relocate nothing) the GC reads the graph store
// alone, so a v1 molecule root in a rig store is never closed or purged.
func TestWispGCNeverReadsARigOnAnUnsplitCity(t *testing.T) {
	s := gcRecipes(t)["unsplit"]()
	seedGC(t, "unsplit", s)
	before := gcRows(t, gcShape{stores: map[string]beads.Store{"alpha": s.stores["alpha"], "bravo": s.stores["bravo"]}})
	withWorkMoleculesEnforced(t, func() {
		withCloseAbandonedEnforced(t, func() {
			wg := newTestGC()
			now := time.Now().Add(48 * time.Hour)
			_, _ = wg.wg.runGC(s.roots(), nil, beads.GraphStore{Store: s.graph}, beads.SessionStore{}, beads.MailStore{}, now)
		})
	})
	after := gcRows(t, gcShape{stores: map[string]beads.Store{"alpha": s.stores["alpha"], "bravo": s.stores["bravo"]}})
	if after != before {
		t.Fatalf("the unsplit GC touched the rigs:\n before %s\n after  %s", before, after)
	}
}

// TestWispGCFailsClosedOnACensusTheEpochDisagreesWith: a census leg the
// epoch resolves to another store is not read: the work arm skips it (no
// close, no purge there), and a root it might hold is held. A graph leg the
// epoch resolves to another store lists nothing and purges nothing.
func TestWispGCFailsClosedOnACensusTheEpochDisagreesWith(t *testing.T) {
	s := gcRecipes(t)["whole split"]()
	seedGC(t, "whole split", s)
	other := newTestMem("ra")
	s.epoch = testStoresWithRigs(s.stores["city"], s.graph, map[string]testRig{"alpha": {store: other, prefix: "ra"}, "bravo": {store: s.stores["bravo"], prefix: "rb"}})
	roots := s.roots()
	if _, err := roots.workLeg("rig:alpha"); err == nil {
		t.Fatal("a census leg the epoch resolves elsewhere has a reader")
	}
	h := wispGCHolders{graph: s.graph, roots: roots, legs: s.legs}
	if !h.readHolders(beads.Bead{ID: "gcg-free"}, false, nil, "") {
		t.Error("a root whose holders sit behind an unreadable leg was answered not held")
	}
	alphaBefore := gcRows(t, gcShape{stores: map[string]beads.Store{"alpha": s.stores["alpha"]}})
	withWorkMoleculesEnforced(t, func() {
		withCloseAbandonedEnforced(t, func() {
			wg := newTestGC()
			now := time.Now().Add(48 * time.Hour)
			if _, err := wg.work.pass(context.Background(), roots, s.legs, s.graph, time.Hour, now); err == nil {
				t.Error("the work arm reported no error for a leg it could not read")
			}
		})
	})
	if after := gcRows(t, gcShape{stores: map[string]beads.Store{"alpha": s.stores["alpha"]}}); after != alphaBefore {
		t.Errorf("the work arm wrote to a leg it did not read:\n before %s\n after  %s", alphaBefore, after)
	}
	// The graph leg resolved elsewhere.
	s.epoch = testStoresWithRigs(s.stores["city"], newTestMem("gcg"), nil)
	roots = s.roots()
	if _, err := roots.graphReader(); err == nil {
		t.Fatal("a graph leg the epoch resolves elsewhere has a reader")
	}
	graphBefore := gcRows(t, gcShape{stores: map[string]beads.Store{"graph": s.graph}})
	if _, err := newTestGC().wg.runGC(roots, s.legs, beads.GraphStore{Store: s.graph}, beads.SessionStore{}, beads.MailStore{}, time.Now().Add(48*time.Hour)); err == nil {
		t.Error("the graph arm reported no error without a graph reader")
	}
	if after := gcRows(t, gcShape{stores: map[string]beads.Store{"graph": s.graph}}); after != graphBefore {
		t.Errorf("the graph arm wrote without a reader:\n before %s\n after  %s", graphBefore, after)
	}
}

// TestWispGCHolderErrorMeansHeld (N5): a holder leg that fails to list is
// "held" in the visitor, whatever the plan's leg policy (the work legs are
// PartialDegrade).
func TestWispGCHolderErrorMeansHeld(t *testing.T) {
	s := gcRecipes(t)["whole split"]()
	seedGC(t, "whole split", s)
	roots := s.roots()
	roots.work["rig:alpha"] = beadstestFederatedLegOver(&listErrStore{Store: s.stores["alpha"], err: errors.New("leg down")})
	h := wispGCHolders{graph: s.graph, roots: roots, legs: s.legs}
	if !h.readHolders(beads.Bead{ID: "gcg-free"}, false, nil, "") {
		t.Fatal("a holder leg that failed to list answered not held")
	}
}

// beadstestFederatedLegOver is a federated leg over s, the test mint's.
func beadstestFederatedLegOver(s beads.Store) beads.FederatedReader { return beadstest.FederatedLeg(s) }

func gcRefHasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// TestTickWispGCReadsHoldersThroughTheCensusLegs: the tick hands the GC the
// census legs' readers on a city whose routes relocate graph, so the holder
// guard reads a rig's holders: a stepless wisp whose only source, in the rig,
// is terminal closes. Without the rig's reader the guard would answer "held"
// and keep it open forever.
func TestTickWispGCReadsHoldersThroughTheCensusLegs(t *testing.T) {
	cr, e := laneRuntime(t, true)
	graph := e.graphStore()
	root := fedCreate(t, graph, beads.Bead{Title: "stepless wisp", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	rig := cr.standaloneRigStores[e.rigName]
	src, err := rig.Create(beads.Bead{Title: "done source", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.Close(src.ID); err != nil {
		t.Fatal(err)
	}
	withCloseAbandonedEnforced(t, func() {
		// A negative idle TTL puts the cutoff ahead of now: the root just
		// created counts as idle, with no wait.
		withCloseAbandonedTTL(t, -time.Hour, func() {
			cr.tickWispGC(&tickPass{})
		})
	})
	if got, err := graph.Get(root.ID); err == nil && got.Status != "closed" {
		t.Fatalf("stepless wisp %s is %s with its only (rig) source terminal; stderr: %s", root.ID, got.Status, cr.stderr.(*bytes.Buffer).String())
	}
}
