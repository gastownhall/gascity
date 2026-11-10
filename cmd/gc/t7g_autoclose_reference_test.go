package main

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// Typed-stores T7g part 2 (t7g-design.md §4): the autoclose federations
// planned by storeref intents. Each converted site's pre-T7g body is frozen
// below, and an equivalence table runs it and the converted code over the
// same shape corpus, built twice from one recipe so each side mutates its own
// copy: unsplit, whole split, a rig, a suspended rig, a refused binding, a
// relic-proven binding (a work-prefixed id resident in the binding), a
// co-resident id, the gate's views (gate on), and a stranded split served
// degraded (#9025: the work ledger holds a graph-class row).

// --- frozen references ---

func autocloseLocateReference(id string, first, second beads.Store) (beads.Bead, beads.Store, error) {
	b, err := first.Get(id)
	if err == nil {
		return b, first, nil
	}
	if !errors.Is(err, beads.ErrNotFound) || second == nil || sameBeadStore(first, second) {
		return beads.Bead{}, first, err
	}
	b, err = second.Get(id)
	if err != nil {
		return beads.Bead{}, second, err
	}
	return b, second, nil
}

func doMoleculeAutocloseWithReference(store beads.Store, rec events.Recorder, beadID string, graphStore beads.Store) autocloseRun {
	var run autocloseRun
	if graphStore == nil {
		graphStore = store
	}
	bead, err := store.Get(beadID)
	if err != nil {
		run.note(err)
		return run
	}
	autocloseRootsForSourceBead(graphStore, "", rec, beadID, io.Discard, &run)
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		if bead.Type != "step" || bead.ParentID == "" {
			return run
		}
		parent, parentStore, err := autocloseLocateReference(bead.ParentID, store, graphStore)
		if err != nil {
			run.note(err)
			return run
		}
		autocloseMoleculeIfComplete(parentStore, rec, parent, io.Discard, &run)
		return run
	}
	root, rootStore, err := autocloseLocateReference(rootID, graphStore, store)
	if err != nil {
		run.note(err)
		return run
	}
	autocloseMoleculeIfComplete(rootStore, rec, root, io.Discard, &run)
	return run
}

// convoyHomeReference is the pre-T7g convoyAutocloseHomes.parent: nil when
// the row's store is a work store, else the rig whose prefix the id carries
// longest, else the city; none for a reserved id.
func convoyHomeReference(store, city beads.Store, rigs map[string]beads.Store, prefixes map[string]string) func(string) beads.Store {
	if store == nil || sameBeadStore(store, city) {
		return nil
	}
	for _, rig := range rigs {
		if sameBeadStore(store, rig) {
			return nil
		}
	}
	type rigPrefix struct{ prefix, name string }
	var ps []rigPrefix
	for name, p := range prefixes {
		if p != "" {
			ps = append(ps, rigPrefix{p + "-", name})
		}
	}
	sort.SliceStable(ps, func(i, j int) bool { return len(ps[i].prefix) > len(ps[j].prefix) })
	return func(id string) beads.Store {
		if bdIDIsClassReserved(id) {
			return nil
		}
		for _, p := range ps {
			if strings.HasPrefix(id, p.prefix) {
				if rig := rigs[p.name]; rig != nil {
					return rig
				}
			}
		}
		return city
	}
}

func closeCompletedConvoyAcrossReference(store, childStore beads.Store, convoy beads.Bead) (bool, error) {
	if !convoyStillAutocloses(convoy) {
		return false, nil
	}
	across := childStore != nil && !sameBeadStore(childStore, store)
	children, err := listConvoyChildren(store, convoy.ID, true)
	if err != nil {
		return false, fmt.Errorf("children of %s: %w", convoy.ID, err)
	}
	for _, ch := range children {
		if !convoycore.IsTerminalStatus(ch.Status) {
			return false, nil
		}
	}
	if across {
		elsewhere, err := childStore.List(beads.ListQuery{ParentID: convoy.ID, IncludeClosed: true, Sort: beads.SortCreatedAsc, TierMode: beads.FederatedReadTier})
		if err != nil {
			return false, fmt.Errorf("children of %s: %w", convoy.ID, err)
		}
		for _, ch := range elsewhere {
			if !convoycore.IsTerminalStatus(ch.Status) {
				return false, nil
			}
		}
		children = append(children, elsewhere...)
	}
	if len(children) == 0 {
		return false, nil
	}
	closed, err := autocloseCloseIfStill(store, convoy.ID, convoyAutocloseReason, convoyStillAutocloses, func() error {
		return closeConvoyWithReason(store, convoy.ID, convoyAutocloseReason)
	})
	if err != nil {
		return false, fmt.Errorf("closing %s: %w", convoy.ID, err)
	}
	return closed, nil
}

func doConvoyAutocloseWithReference(store beads.Store, parentHome func(string) beads.Store, children beads.Store, beadID string) autocloseRun {
	var run autocloseRun
	bead, err := store.Get(beadID)
	if err != nil {
		run.note(err)
		return run
	}
	homes := convoyAutocloseHomes{children: children}
	seen := map[string]bool{}
	if bead.ParentID != "" {
		var home beads.Store
		if parentHome != nil {
			home = parentHome(bead.ParentID)
		}
		parent, parentStore, err := autocloseLocateReference(bead.ParentID, store, home)
		run.note(err)
		if err == nil {
			seen[parent.ID] = true
			closed, err := closeCompletedConvoyAcrossReference(parentStore, convoyChildStore(parentStore, store, homes), parent)
			run.note(err)
			_ = closed
		}
	}
	tracking, err := convoycore.TrackingConvoysForItem(beads.WorkStore{Store: store}, beadID)
	if err != nil {
		run.note(err)
		return run
	}
	for _, convoy := range tracking {
		if seen[convoy.ID] {
			continue
		}
		seen[convoy.ID] = true
		_, err := closeCompletedConvoyAcrossReference(store, convoyChildStore(store, store, homes), convoy)
		run.note(err)
	}
	return run
}

func collectInputConvoyWorkflowRootsReference(workStore, graphStore beads.Store, parent beads.Bead, seen map[string]bool) []beads.Bead {
	convoys, err := convoycore.TrackingConvoysForItem(beads.WorkStore{Store: workStore}, parent.ID)
	if err != nil {
		return nil
	}
	if graphStore != workStore {
		graphConvoys, err := convoycore.TrackingConvoysForItem(beads.WorkStore{Store: graphStore}, parent.ID)
		if err != nil {
			return nil
		}
		seenIDs := map[string]bool{}
		var merged []beads.Bead
		for _, list := range [][]beads.Bead{convoys, graphConvoys} {
			for _, c := range list {
				if !seenIDs[c.ID] {
					seenIDs[c.ID] = true
					merged = append(merged, c)
				}
			}
		}
		convoys = merged
	}
	var roots []beads.Bead
	for _, convoy := range convoys {
		matches, err := graphStore.ListByMetadata(map[string]string{beadmeta.InputConvoyIDMetadataKey: convoy.ID}, 0, beads.WithBothTiers)
		if err != nil {
			continue
		}
		for _, root := range matches {
			if seen[root.ID] || convoycore.IsTerminalStatus(root.Status) || root.Metadata[beadmeta.FormulaContractMetadataKey] != "graph.v2" || root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow {
				continue
			}
			seen[root.ID] = true
			roots = append(roots, root)
		}
	}
	return roots
}

// --- the shape corpus ---

// fedShape is one shape's stores, as a recipe builds them: city is the city
// work store, binding the graph-relocating binding (nil: unsplit), rig the
// one rig ("ra" prefix; nil: none). epoch is the epoch over them, and the
// raw stores each reference reads are the epoch's own (gate views on the
// gated shape).
type fedShape struct {
	name               string
	city, rig, binding beads.Store
	graph              beads.Store // the epoch's graph store
	epoch              storeref.Stores
}

// shapeRecipe builds one shape, fresh each call.
type shapeRecipe struct {
	name  string
	build func(t *testing.T) fedShape
}

func newTestMem(prefix string) *beads.MemStore {
	m := beads.NewMemStore()
	m.HonorExplicitIDs = true
	if prefix != "" {
		m.IDPrefix = prefix
	}
	return m
}

func epochOver(city, binding beads.Store, rig beads.Store, suspended bool, opts storeref.StoresOptions) storeref.Stores {
	topo := storeref.Topology{Work: storeref.Leg{Ref: storeref.WorkRef, Store: city, Prefix: "ga"}}
	if rig != nil {
		topo.Rigs = []storeref.Leg{{Ref: storeref.RigRef("alpha"), Store: rig, Prefix: "ra", Suspended: suspended}}
	}
	if binding != nil {
		var classes []coordclass.Class
		for _, c := range coordclass.Classes() {
			if c.IsInfrastructure() {
				classes = append(classes, c)
			}
		}
		topo.Bindings, _ = storeref.BuildBinding(binding, classes, storeref.BindingOptions{})
	}
	return storeref.NewStores(topo, opts)
}

func fedShapes() []shapeRecipe {
	plain := func(name string, split, withRig, suspended, refused bool) shapeRecipe {
		return shapeRecipe{name, func(*testing.T) fedShape {
			s := fedShape{name: name, city: newTestMem("ga")}
			if withRig {
				s.rig = newTestMem("ra")
			}
			if split {
				s.binding = newTestMem("gcg")
				if refused {
					s.binding = refusedClassStore{err: standingStorageRefusal{err: errors.New("storage refused: run `gc storage migrate`")}}
				}
			}
			s.epoch = epochOver(s.city, s.binding, s.rig, suspended, storeref.StoresOptions{})
			s.graph = s.epoch.Graph().Store
			return s
		}}
	}
	return []shapeRecipe{
		plain("unsplit", false, false, false, false),
		plain("unsplit with a rig", false, true, false, false),
		plain("whole split", true, false, false, false),
		plain("split with a rig", true, true, false, false),
		plain("split, suspended rig", true, true, true, false),
		plain("refused", true, true, false, true),
		// relic-proven, co-resident and stranded-degraded are split shapes
		// whose rows the seed places (seedFor): a work-prefixed id in the
		// binding, one id in both stores, a graph-class row on the work
		// ledger while the split serves.
		plain("relic-proven", true, true, false, false),
		plain("co-resident id", true, true, false, false),
		plain("stranded, served degraded", true, true, false, false),
		{"gate on", func(*testing.T) fedShape {
			physical := newTestMem("ga")
			epoch := epochOver(physical, nil, nil, false, storeref.StoresOptions{Projection: splitStrictGate{}})
			return fedShape{name: "gate on", city: epoch.Work().Store, epoch: epoch, graph: epoch.Graph().Store}
		}},
	}
}

func fedCreate(t *testing.T, s beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	got, err := s.Create(b)
	if err != nil {
		t.Fatalf("seeding %s: %v", b.ID, err)
	}
	return got
}

// statusesOf is every row's status across the shape's stores, for comparing
// what two runs left behind.
func statusesOf(t *testing.T, s fedShape) string {
	t.Helper()
	var out []string
	for name, st := range map[string]beads.Store{"city": s.city, "rig": s.rig, "binding": s.binding} {
		if st == nil {
			continue
		}
		rows, err := st.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			out = append(out, name+": "+err.Error())
			continue
		}
		for _, b := range rows {
			out = append(out, fmt.Sprintf("%s/%s=%s", name, b.ID, b.Status))
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func runOutcome(r autocloseRun) string {
	return fmt.Sprintf("refused=%v failed=%v", r.refused, r.failed)
}

// TestMoleculeAutocloseAgreesWithTheFrozenReference: doMoleculeAutocloseWith
// over the epoch's Molecule{RootNear, ParentNear} plans closes what the
// pre-T7g body closed, and reports the same run, on every shape: a step
// under a graph root (metadata) and a legacy step under a v1 poured molecule
// (parent), closed in the binding or on the work ledger, with the root or
// parent in either store, both, or neither.
func TestMoleculeAutocloseAgreesWithTheFrozenReference(t *testing.T) {
	type seed struct {
		name    string
		closeIn func(s fedShape) beads.Store // the closed step's store
		rootIn  func(s fedShape) []beads.Store
		legacy  bool
	}
	graphOf := func(s fedShape) beads.Store { return s.graph }
	cityOf := func(s fedShape) beads.Store { return s.city }
	seeds := []seed{
		{"graph root, step in graph", graphOf, func(s fedShape) []beads.Store { return []beads.Store{s.graph} }, false},
		{"graph root, step on the work ledger", cityOf, func(s fedShape) []beads.Store { return []beads.Store{s.graph} }, false},
		{"root on the work ledger only", cityOf, func(s fedShape) []beads.Store { return []beads.Store{s.city} }, false},
		{"root in both (co-resident id)", cityOf, func(s fedShape) []beads.Store { return []beads.Store{s.graph, s.city} }, false},
		{"root nowhere", cityOf, func(fedShape) []beads.Store { return nil }, false},
		{"legacy parent beside the step", cityOf, func(s fedShape) []beads.Store { return []beads.Store{s.city} }, true},
		{"legacy parent in graph only", cityOf, func(s fedShape) []beads.Store { return []beads.Store{s.graph} }, true},
		{"legacy parent in both", graphOf, func(s fedShape) []beads.Store { return []beads.Store{s.graph, s.city} }, true},
	}
	for _, recipe := range fedShapes() {
		for _, sd := range seeds {
			name := recipe.name + "/" + sd.name
			t.Run(name, func(t *testing.T) {
				plant := func(s fedShape) string {
					const rootID, stepID = "ga-root", "ga-step"
					for _, st := range sd.rootIn(s) {
						if st == nil {
							continue
						}
						if _, err := st.Get(rootID); err == nil {
							continue
						}
						if _, err := st.Create(beads.Bead{ID: rootID, Title: "root", Type: "molecule"}); err != nil {
							t.Skipf("the shape cannot hold the root there: %v", err)
						}
					}
					step := beads.Bead{ID: stepID, Title: "step", Type: "step"}
					if sd.legacy {
						step.ParentID = rootID
					} else {
						step.Metadata = map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}
					}
					in := sd.closeIn(s)
					if _, err := in.Create(step); err != nil {
						t.Skipf("the shape cannot hold the step there: %v", err)
					}
					if err := in.Close(stepID); err != nil {
						t.Fatal(err)
					}
					return stepID
				}
				ref, conv := recipe.build(t), recipe.build(t)
				refID, convID := plant(ref), plant(conv)
				want := runOutcome(doMoleculeAutocloseWithReference(sd.closeIn(ref), events.Discard, refID, ref.graph))
				got := runOutcome(doMoleculeAutocloseWith(sd.closeIn(conv), "", events.Discard, convID, io.Discard, conv.epoch))
				if got != want {
					t.Errorf("run = %s, reference %s", got, want)
				}
				if g, w := statusesOf(t, conv), statusesOf(t, ref); g != w {
					t.Errorf("rows after the run:\n got  %s\n want %s", g, w)
				}
			})
		}
	}
}

// TestConvoyAutocloseAgreesWithTheFrozenReference: doConvoyAutocloseWith over
// the epoch's Convoy(Parent) plan and Convoy(Children) walk closes what the
// pre-T7g body (its parent home table and two-store children read) closed,
// on every shape: a class row under a work convoy in the city or the rig, a
// work row under a convoy beside it, a reserved parent id, an unreadable
// home, and a convoy whose binding child is still open.
func TestConvoyAutocloseAgreesWithTheFrozenReference(t *testing.T) {
	type seed struct {
		name         string
		parentID     string
		convoyIn     func(s fedShape) beads.Store
		rowIn        func(s fedShape) beads.Store
		openChildIn  func(s fedShape) beads.Store // nil: none
		homeFailures bool
	}
	cityOf := func(s fedShape) beads.Store { return s.city }
	rigOf := func(s fedShape) beads.Store { return s.rig }
	graphOf := func(s fedShape) beads.Store { return s.graph }
	seeds := []seed{
		{name: "class row under a city convoy", parentID: "ga-convoy", convoyIn: cityOf, rowIn: graphOf},
		{name: "class row under a rig convoy", parentID: "ra-convoy", convoyIn: rigOf, rowIn: graphOf},
		{name: "work row under a convoy beside it", parentID: "ga-convoy", convoyIn: cityOf, rowIn: cityOf},
		{name: "class row, reserved parent id", parentID: "gcg-convoy", convoyIn: cityOf, rowIn: graphOf},
		{name: "class row under a convoy with an open binding child", parentID: "ga-convoy", convoyIn: cityOf, rowIn: cityOf, openChildIn: graphOf},
		{name: "class row, the home cannot be read", parentID: "ga-convoy", convoyIn: cityOf, rowIn: graphOf, homeFailures: true},
	}
	for _, recipe := range fedShapes() {
		for _, sd := range seeds {
			t.Run(recipe.name+"/"+sd.name, func(t *testing.T) {
				plant := func(s fedShape) (fedShape, string) {
					convoyStore, rowStore := sd.convoyIn(s), sd.rowIn(s)
					if beads.IsNilStore(convoyStore) || beads.IsNilStore(rowStore) {
						t.Skip("the shape has no such store")
					}
					if _, err := convoyStore.Create(beads.Bead{ID: sd.parentID, Title: "convoy", Type: "convoy"}); err != nil {
						t.Skipf("the shape cannot hold the convoy there: %v", err)
					}
					if sd.openChildIn != nil && !beads.IsNilStore(sd.openChildIn(s)) {
						if _, err := sd.openChildIn(s).Create(beads.Bead{ID: "gcg-open", Title: "open child", Type: "task", ParentID: sd.parentID}); err != nil {
							t.Skipf("cannot seed the open child: %v", err)
						}
					}
					if _, err := rowStore.Create(beads.Bead{ID: "gcg-row", Title: "row", Type: "task", ParentID: sd.parentID}); err != nil {
						t.Skipf("the shape cannot hold the row there: %v", err)
					}
					if err := rowStore.Close("gcg-row"); err != nil {
						t.Fatal(err)
					}
					if sd.homeFailures {
						s.city = &getErrStore{Store: s.city, err: errors.New("home down")}
						s.epoch = epochOver(s.city, s.binding, s.rig, false, storeref.StoresOptions{})
					}
					return s, "gcg-row"
				}
				ref, conv := recipe.build(t), recipe.build(t)
				ref, refID := plant(ref)
				conv, convID := plant(conv)
				var children beads.Store
				if ref.binding != nil && !sameBeadStore(ref.graph, ref.city) {
					children = ref.graph
				}
				rigs := map[string]beads.Store{}
				if ref.rig != nil {
					rigs["alpha"] = ref.rig
				}
				home := convoyHomeReference(sd.rowIn(ref), ref.city, rigs, map[string]string{"alpha": "ra"})
				want := runOutcome(doConvoyAutocloseWithReference(sd.rowIn(ref), home, children, refID))
				var convChildren beads.Store
				if conv.binding != nil && !sameBeadStore(conv.graph, conv.city) && !sameBeadStore(conv.graph, sd.rowIn(conv)) {
					convChildren = conv.graph
				}
				got := runOutcome(doConvoyAutocloseWith(sd.rowIn(conv), conv.epoch, convoyAutocloseHomes{children: convChildren}, events.Discard, convID, io.Discard, io.Discard))
				if got != want {
					t.Errorf("run = %s, reference %s", got, want)
				}
				if g, w := statusesOf(t, conv), statusesOf(t, ref); g != w {
					t.Errorf("rows after the run:\n got  %s\n want %s", g, w)
				}
			})
		}
	}
}

// secondStoreLists counts List calls.
type secondStoreLists struct {
	beads.Store
	lists *int
	err   error
}

func (s secondStoreLists) List(q beads.ListQuery) ([]beads.Bead, error) {
	*s.lists++
	if s.err != nil {
		return nil, s.err
	}
	return s.Store.List(q)
}

// TestCloseCompletedConvoyAcrossAgreesWithTheFrozenReference: the
// Convoy(Children) walk decides and closes as the pre-T7g two-store read did
// (an open child in either store keeps the convoy open; none anywhere is no
// close), reads the second store only once every first-store child is
// terminal (lazy), and keeps its error text (N3: gc convoy check prints it).
func TestCloseCompletedConvoyAcrossAgreesWithTheFrozenReference(t *testing.T) {
	type tc struct {
		name                   string
		firstOpen, secondOpen  bool
		firstChild, secondKids bool
		secondErr              error
		same                   bool
		nilSecond              bool
	}
	cases := []tc{
		{name: "every child terminal in both", firstChild: true, secondKids: true},
		{name: "an open child in the first store", firstChild: true, firstOpen: true, secondKids: true},
		{name: "an open child in the second store", firstChild: true, secondKids: true, secondOpen: true},
		{name: "children only in the second store", secondKids: true},
		{name: "no children anywhere"},
		{name: "the second store fails", firstChild: true, secondErr: errors.New("binding down")},
		{name: "the second store fails behind an open first child", firstChild: true, firstOpen: true, secondErr: errors.New("binding down")},
		{name: "the second store is the first", firstChild: true, same: true},
		{name: "no second store", firstChild: true, nilSecond: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			build := func() (beads.Store, beads.Store, *int, beads.Bead) {
				first, secondMem := newTestMem("ga"), newTestMem("gcg")
				convoy := fedCreate(t, first, beads.Bead{ID: "ga-c", Title: "convoy", Type: "convoy"})
				if c.firstChild {
					fedCreate(t, first, beads.Bead{ID: "ga-k", Title: "child", Type: "task", ParentID: convoy.ID})
					if !c.firstOpen {
						_ = first.Close("ga-k")
					}
				}
				if c.secondKids {
					fedCreate(t, secondMem, beads.Bead{ID: "gcg-k", Title: "child", Type: "task", ParentID: convoy.ID})
					if !c.secondOpen {
						_ = secondMem.Close("gcg-k")
					}
				}
				lists := 0
				var second beads.Store = secondStoreLists{Store: secondMem, lists: &lists, err: c.secondErr}
				switch {
				case c.same:
					second = first
				case c.nilSecond:
					second = nil
				}
				return first, second, &lists, convoy
			}
			rf, rs, rlists, rconvoy := build()
			wantClosed, wantErr := closeCompletedConvoyAcrossReference(rf, rs, rconvoy)
			cf, cs, clists, cconvoy := build()
			gotClosed, gotErr := closeCompletedConvoyAcross(testStores(cf, nil), cf, cs, cconvoy)
			if gotClosed != wantClosed || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Errorf("closeCompletedConvoyAcross = %v, %v; reference %v, %v", gotClosed, gotErr, wantClosed, wantErr)
			}
			if *clists != *rlists {
				t.Errorf("second-store lists = %d, reference %d (the walk is lazy)", *clists, *rlists)
			}
			rb, _ := rf.Get(rconvoy.ID)
			cb, _ := cf.Get(cconvoy.ID)
			if rb.Status != cb.Status {
				t.Errorf("convoy %s after the call, reference %s", cb.Status, rb.Status)
			}
		})
	}
}

// TestCollectInputConvoyRootsAgreesWithTheFrozenReference: the
// Convoy(Tracking) union finds the input-convoy roots the pre-T7g two-store
// probe found, on every shape, a tracking convoy on either store or both,
// and declines (no roots) when either leg cannot be read: it still fails
// closed.
func TestCollectInputConvoyRootsAgreesWithTheFrozenReference(t *testing.T) {
	for _, recipe := range fedShapes() {
		for _, where := range []string{"work", "graph", "both", "none", "work fails", "graph fails", "both, graph fails"} {
			t.Run(recipe.name+"/"+where, func(t *testing.T) {
				run := func(ref bool) []string {
					s := recipe.build(t)
					work := s.city
					item := fedCreate(t, work, beads.Bead{ID: "ga-item", Title: "issue", Type: "task"})
					seedConvoy := func(st beads.Store, id string) {
						if _, err := st.Create(beads.Bead{ID: id, Title: "input convoy", Type: "convoy"}); err != nil {
							t.Skipf("cannot seed the convoy: %v", err)
						}
						if err := st.DepAdd(id, item.ID, convoycore.TrackingDepType); err != nil {
							t.Skipf("cannot seed the tracks edge: %v", err)
						}
						if _, err := s.graph.Create(beads.Bead{ID: "gcg-root-" + id, Title: "root", Type: "task", Metadata: map[string]string{
							beadmeta.InputConvoyIDMetadataKey:   id,
							beadmeta.FormulaContractMetadataKey: "graph.v2",
							beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
						}}); err != nil && !storeref.IsStandingRefusal(err) {
							t.Skipf("cannot seed the root: %v", err)
						}
					}
					switch where {
					case "work", "work fails":
						seedConvoy(work, "ga-cw")
					case "graph", "graph fails":
						seedConvoy(s.graph, "gcg-cg")
					case "both", "both, graph fails":
						seedConvoy(work, "ga-cw")
						seedConvoy(s.graph, "gcg-cg")
					}
					graph := s.graph
					epoch := s.epoch
					switch where {
					case "work fails":
						work = failingDepListStore{Store: work}
						epoch = epochOver(work, s.binding, s.rig, false, storeref.StoresOptions{})
						if s.binding == nil {
							graph = work
						}
					case "graph fails", "both, graph fails":
						if sameBeadStore(graph, work) {
							t.Skip("one store: no separate graph leg to fail")
						}
						graph = failingDepListStore{Store: graph}
						epoch = epochOver(work, graph, s.rig, false, storeref.StoresOptions{})
					}
					var roots []beads.Bead
					if ref {
						roots = collectInputConvoyWorkflowRootsReference(work, graph, item, map[string]bool{})
					} else {
						roots = collectInputConvoyWorkflowRoots(epoch, work, epoch.Graph().Store, item, map[string]bool{})
					}
					var ids []string
					for _, r := range roots {
						ids = append(ids, r.ID)
					}
					return ids
				}
				got, want := run(false), run(true)
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("roots = %s, reference %s", got, want)
				}
				if strings.HasSuffix(where, "fails") && len(got) != 0 {
					t.Errorf("a failed leg still yielded roots %v: the read must fail closed", got)
				}
			})
		}
	}
}

// failingDepListStore fails DepList: a tracking-convoy leg that cannot be
// read.
type failingDepListStore struct{ beads.Store }

func (failingDepListStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New("leg down")
}

// reasonCloser records CloseWithReason, the bd store's explicit-reason close.
type reasonCloser struct {
	*beads.MemStore
	reasons map[string]string
}

func (r *reasonCloser) CloseWithReason(id, reason string) error {
	r.reasons[id] = reason
	return r.Close(id)
}

// TestMoleculeAutocloseClosesThroughTheResidentHandle pins the close of a
// root the Molecule plan resolved: it goes through the plan's resident
// handle, and a store with an explicit-reason close is still asked for it
// (closeMoleculeWithReason finds it below the handle, beads.As), and a
// CachingStore under the handle takes the fenced close (R8).
func TestMoleculeAutocloseClosesThroughTheResidentHandle(t *testing.T) {
	seed := func(t *testing.T, s beads.Store) {
		fedCreate(t, s, beads.Bead{ID: "ga-root", Title: "root", Type: "molecule"})
		fedCreate(t, s, beads.Bead{ID: "ga-step", Title: "step", Type: "step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "ga-root"}})
		if err := s.Close("ga-step"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("explicit-reason closer", func(t *testing.T) {
		store := &reasonCloser{MemStore: newTestMem("ga"), reasons: map[string]string{}}
		seed(t, store)
		doMoleculeAutocloseWith(store, "", events.Discard, "ga-step", io.Discard, testStores(store, store))
		if got := store.reasons["ga-root"]; got != moleculeAutocloseReason {
			t.Fatalf("CloseWithReason for the root = %q, want %q", got, moleculeAutocloseReason)
		}
	})
	t.Run("a CachingStore under the handle", func(t *testing.T) {
		backing := newTestMem("ga")
		seed(t, backing)
		cache := beads.NewCachingStoreForTest(backing, nil)
		run := doMoleculeAutocloseWith(cache, "", events.Discard, "ga-step", io.Discard, testStores(cache, cache))
		if !run.finished() {
			t.Fatalf("run = %s, want finished", runOutcome(run))
		}
		if b, _ := backing.Get("ga-root"); b.Status != "closed" {
			t.Fatalf("root %s in the backing store, want closed through the resident handle", b.Status)
		}
	})
	t.Run("the fenced close CASes through a resident over a CachingStore", func(t *testing.T) {
		for _, wrap := range []struct {
			name string
			fn   func(beads.Store) beads.Store
		}{
			{"the cache", func(s beads.Store) beads.Store { return s }},
			{"a resident over the cache", func(s beads.Store) beads.Store { return beadstest.Resident(s) }},
		} {
			backing := beads.NewMemStore()
			stampedMemStore(t, backing)
			target := fedCreate(t, backing, beads.Bead{Title: "root", Type: "molecule"})
			cache := beads.NewCachingStoreForTest(backing, nil)
			unconditional := 0
			closed, err := autocloseCloseIfStill(wrap.fn(cache), target.ID, moleculeAutocloseReason, func(b beads.Bead) bool { return b.Status != "closed" }, func() error {
				unconditional++
				return cache.Close(target.ID)
			})
			got, _ := backing.Get(target.ID)
			if err != nil || !closed || unconditional != 0 || got.Status != "closed" || got.Metadata["close_reason"] != moleculeAutocloseReason {
				t.Errorf("%s: closed=%v err=%v unconditional=%d status=%q reason=%q; want a CAS close with its reason", wrap.name, closed, err, unconditional, got.Status, got.Metadata["close_reason"])
			}
		}
	})
	t.Run("the handle resolves the cache's fence", func(t *testing.T) {
		cache := beads.NewCachingStoreForTest(newTestMem("ga"), nil)
		handle := beadstest.Resident(cache)
		w1, _, err1 := beads.ResolveConditionalWriter(handle)
		w2, _, err2 := beads.ResolveConditionalWriter(cache)
		if (w1 == nil) != (w2 == nil) || (err1 == nil) != (err2 == nil) {
			t.Fatalf("a resident handle over the cache resolves writer=%v err=%v, the cache writer=%v err=%v", w1 != nil, err1, w2 != nil, err2)
		}
		_, a1 := beads.AtomicConditionalCloserFor(handle)
		_, a2 := beads.AtomicConditionalCloserFor(cache)
		if a1 != a2 {
			t.Fatalf("a resident handle over the cache resolves atomic closer %v, the cache %v", a1, a2)
		}
	})
}
