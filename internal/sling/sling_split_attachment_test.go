package sling

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/runtime"
)

// This file pins the legacy (non-graph.v2) --on attach guard on a split city.
//
// A sling pours its molecule through deps.graphStore(). On a city whose graph
// class is relocated to a [storage] binding, that is a different store from
// deps.Store, which holds the source bead and its molecule_id stamp. The guard
// used to read the molecule back through deps.Store, could not see it, and
// failed open: a re-sling poured a second live molecule beside the first, an
// unassigned source's stale molecule was never burned, and a routed bead's
// idempotency probe reported "could not verify" and dropped the request.
//
// Every scenario runs on both topologies. The split leg is the production
// shape: the work leaf is the kit's strict bd-semantics store, and the graph
// leg is a real beads.SQLiteStore under the graph class's reserved prefix,
// fenced to its namespaces, which is what the sqlite-beads binding serves.

const (
	splitAttachVaporA  = "split-vapor-a"
	splitAttachVaporB  = "split-vapor-b"
	splitAttachPouredA = "split-poured-a"
	splitAttachPouredB = "split-poured-b"
)

// splitAttachFormulaDir writes two v1 vapor (root-only wisp) formulas and two
// v1 poured formulas into a fresh formula layer.
func splitAttachFormulaDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{splitAttachVaporA, splitAttachVaporB} {
		body := fmt.Sprintf("formula = %q\nversion = 1\nphase = \"vapor\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n", name)
		if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{splitAttachPouredA, splitAttachPouredB} {
		body := fmt.Sprintf("formula = %q\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n", name)
		if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// realSQLiteGraphLeg opens the graph leg the way the sqlite-beads binding does:
// a real beads.SQLiteStore minting the graph class's reserved prefix, fenced to
// the class's namespaces, behind the kit's SQLite-semantics strict wrapper.
func realSQLiteGraphLeg(t *testing.T) beads.Store {
	t.Helper()
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok {
		t.Fatal("no reserved graph prefix")
	}
	namespaces := config.ReservedClassPrefixesFor(config.BeadClassGraph)
	leaf, err := beads.OpenSQLiteStore(t.TempDir(),
		beads.WithSQLiteStoreIDPrefix(prefix),
		beads.WithSQLiteStoreReservedIDPrefixes(namespaces...),
	)
	if err != nil {
		t.Fatalf("opening the sqlite graph leg: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := leaf.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore()
		}
	})
	return splittest.Strict(t, leaf, splittest.SQLiteSemantics, namespaces...)
}

type splitAttachEnv struct {
	split bool
	deps  SlingDeps
	work  beads.Store
	graph beads.Store
}

func newSplitAttachEnv(t *testing.T, split bool) splitAttachEnv {
	t.Helper()
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test"},
		FormulaLayers: config.FormulaLayers{City: []string{splitAttachFormulaDir(t)}},
	}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	env := splitAttachEnv{split: split}
	if split {
		env.work = splittest.NewWorkStore(t, "gc")
		env.graph = realSQLiteGraphLeg(t)
		deps.Store = env.work
		deps.GraphStore = env.graph
	} else {
		env.work = deps.Store
		env.graph = deps.Store
	}
	env.deps = deps
	return env
}

func forEachSlingTopology(t *testing.T, fn func(t *testing.T, env splitAttachEnv)) {
	t.Helper()
	t.Run("single-store", func(t *testing.T) { fn(t, newSplitAttachEnv(t, false)) })
	t.Run("split", func(t *testing.T) { fn(t, newSplitAttachEnv(t, true)) })
}

// liveAttachedRoots counts the open molecule roots in either store whose
// gc.formula_name is one of this file's formulas.
func (env splitAttachEnv) liveAttachedRoots(t *testing.T) []beads.Bead {
	t.Helper()
	stores := []beads.Store{env.graph}
	if env.split {
		stores = append(stores, env.work)
	}
	var live []beads.Bead
	for _, s := range stores {
		rows, err := s.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
		if err != nil {
			t.Fatalf("listing roots: %v", err)
		}
		for _, b := range rows {
			if !strings.HasPrefix(b.Metadata[beadmeta.FormulaNameMetadataKey], "split-") {
				continue
			}
			if b.ParentID != "" || b.Status == "closed" {
				continue
			}
			live = append(live, b)
		}
	}
	return live
}

func (env splitAttachEnv) sling(t *testing.T, beadID, formula string) (SlingResult, error) {
	t.Helper()
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	return DoSling(SlingOpts{Target: a, BeadOrFormula: beadID, OnFormula: formula}, env.deps, env.deps.Store)
}

func (env splitAttachEnv) source(t *testing.T, assignee string) beads.Bead {
	t.Helper()
	b, err := env.work.Create(beads.Bead{Title: "work", Type: "task", Status: "open", Assignee: assignee})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// R2: an assigned source with a live attachment refuses a second --on with a
// MoleculeAttachedError, whatever store the attachment lives in, and no second
// molecule is poured.
func TestSplitSlingOnAssignedSourceRefusesASecondMolecule(t *testing.T) {
	for _, formula := range []string{splitAttachVaporA, splitAttachPouredA} {
		t.Run(formula, func(t *testing.T) {
			forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
				src := env.source(t, "someone")
				first, err := env.sling(t, src.ID, formula)
				if err != nil {
					t.Fatalf("first sling: %v", err)
				}
				if first.WispRootID == "" {
					t.Fatal("first sling poured no molecule")
				}
				_, err = env.sling(t, src.ID, formula)
				var molErr *MoleculeAttachedError
				if !errors.As(err, &molErr) {
					t.Fatalf("second --on on an assigned source with live molecule %s: err=%v, want *MoleculeAttachedError", first.WispRootID, err)
				}
				if molErr.AttachmentID != first.WispRootID {
					t.Errorf("MoleculeAttachedError names %q, want the live attachment %q", molErr.AttachmentID, first.WispRootID)
				}
				if live := env.liveAttachedRoots(t); len(live) != 1 {
					t.Errorf("live attached roots = %d (%v), want exactly 1", len(live), beadIDs(live))
				}
			})
		})
	}
}

// R2: an unassigned source's stale attachment is burned by a re-sling, in the
// store that holds it, and the source then names only the new molecule.
func TestSplitSlingOnUnassignedSourceBurnsTheStaleMolecule(t *testing.T) {
	for _, formula := range []string{splitAttachVaporA, splitAttachPouredA} {
		t.Run(formula, func(t *testing.T) {
			forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
				src := env.source(t, "")
				first, err := env.sling(t, src.ID, formula)
				if err != nil {
					t.Fatalf("first sling: %v", err)
				}
				second, err := env.sling(t, src.ID, formula)
				if err != nil {
					t.Fatalf("second sling: %v", err)
				}
				firstRoot, _, err := getFromStores(first.WispRootID, []beads.Store{env.graph, env.work})
				if err != nil {
					t.Fatalf("reading first root %s: %v", first.WispRootID, err)
				}
				if second.WispRootID != "" && firstRoot.Status != "closed" {
					t.Fatalf("re-sling poured %s while %s is still open (AutoBurned=%v): two live molecules for one source", second.WispRootID, first.WispRootID, second.AutoBurned)
				}
				if live := env.liveAttachedRoots(t); len(live) > 1 {
					t.Errorf("live attached roots = %v, want at most 1", beadIDs(live))
				}
			})
		})
	}
}

// B4: a routed bead with a live attachment that is re-slung with a DIFFERENT
// --on formula must not be dropped as an idempotent no-op: the request either
// errors naming the live attachment, or attaches. Re-slinging the SAME formula
// stays an idempotent no-op on both topologies.
func TestSplitSlingSecondDifferentOnFormulaIsNotSilentlyDropped(t *testing.T) {
	forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
		src := env.source(t, "")
		first, err := env.sling(t, src.ID, splitAttachVaporA)
		if err != nil {
			t.Fatalf("first sling: %v", err)
		}
		if err := env.work.SetMetadata(src.ID, beadmeta.RoutedToMetadataKey, "mayor"); err != nil {
			t.Fatal(err)
		}
		same, err := env.sling(t, src.ID, splitAttachVaporA)
		if err != nil {
			t.Fatalf("re-sling with the same --on: %v", err)
		}
		for _, w := range same.BeadWarnings {
			if strings.Contains(w, "could not verify molecule attachment") {
				t.Errorf("same --on re-sling could not see the live attachment %s: %s", first.WispRootID, w)
			}
		}
		if live := env.liveAttachedRoots(t); len(live) != 1 {
			t.Fatalf("after a same-formula re-sling live attached roots = %v, want exactly 1", beadIDs(live))
		}

		second, err := env.sling(t, src.ID, splitAttachVaporB)
		if err == nil && second.Idempotent {
			t.Fatalf("second --on %s on %s (live attachment %s from %s) returned an idempotent no-op with no error; warnings=%v", splitAttachVaporB, src.ID, first.WispRootID, splitAttachVaporA, second.BeadWarnings)
		}
		var molErr *MoleculeAttachedError
		if err != nil && !errors.As(err, &molErr) {
			t.Fatalf("second --on: err=%v, want *MoleculeAttachedError", err)
		}
		if live := env.liveAttachedRoots(t); len(live) != 1 {
			t.Errorf("live attached roots = %v, want exactly 1", beadIDs(live))
		}
	})
}

// B3: the root-only wisp an attached v1 vapor formula pours keeps its wisp
// marker, so it classifies as graph and is resident in the graph store, while
// staying out of Ready() (it is driven through its source bead).
func TestSplitSlingAttachedVaporWispKeepsItsWispMarker(t *testing.T) {
	forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
		src := env.source(t, "")
		res, err := env.sling(t, src.ID, splitAttachVaporA)
		if err != nil {
			t.Fatalf("sling: %v", err)
		}
		root, err := env.graph.Get(res.WispRootID)
		if err != nil {
			t.Fatalf("attached root %s is not in the graph store: %v", res.WispRootID, err)
		}
		if root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWisp {
			t.Errorf("attached root %s gc.kind=%q, want %q", root.ID, root.Metadata[beadmeta.KindMetadataKey], beadmeta.KindWisp)
		}
		if got := coordclass.Classify(root); got != coordclass.ClassGraph {
			t.Errorf("attached root %s classifies %s, want graph", root.ID, got)
		}
		ready, err := env.graph.Ready()
		if err != nil {
			t.Fatalf("Ready: %v", err)
		}
		for _, b := range ready {
			if b.ID == root.ID {
				t.Errorf("attached wisp root %s is in Ready(); it must be driven through its source bead", root.ID)
			}
		}
	})
}

func beadIDs(bs []beads.Bead) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.ID+"("+b.Status+")")
	}
	return out
}

// Gate-reported misroute (review 3a): a v1 POURED recipe compiles to a
// molecule whose every bead is work class. It is poured into the work store,
// as order dispatch, gc order run and gc formula cook do, both when attached
// with --on and when slung standalone with --formula. A graph-class recipe
// (an attached vapor wisp) still goes to the graph store.
func TestSplitSlingPoursEachRecipeIntoTheStoreOfItsClass(t *testing.T) {
	forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

		src := env.source(t, "")
		poured, err := env.sling(t, src.ID, splitAttachPouredA)
		if err != nil {
			t.Fatalf("sling --on poured: %v", err)
		}
		root, err := env.work.Get(poured.WispRootID)
		if err != nil {
			t.Fatalf("attached v1 poured root %s is not in the work store: %v", poured.WispRootID, err)
		}
		if got := coordclass.Classify(root); got != coordclass.ClassWork {
			t.Fatalf("attached v1 poured root classifies %s, want work", got)
		}

		standalone, err := DoSling(SlingOpts{Target: a, BeadOrFormula: splitAttachPouredB, IsFormula: true}, env.deps, env.deps.Store)
		if err != nil {
			t.Fatalf("sling --formula poured: %v", err)
		}
		if _, err := env.work.Get(standalone.BeadID); err != nil {
			t.Fatalf("standalone v1 poured root %s is not in the work store: %v", standalone.BeadID, err)
		}

		vsrc := env.source(t, "")
		vapor, err := env.sling(t, vsrc.ID, splitAttachVaporA)
		if err != nil {
			t.Fatalf("sling --on vapor: %v", err)
		}
		if _, err := env.graph.Get(vapor.WispRootID); err != nil {
			t.Fatalf("attached vapor wisp %s is not in the graph store: %v", vapor.WispRootID, err)
		}

		if !env.split {
			return
		}
		rows, err := env.graph.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range rows {
			if coordclass.Classify(b) == coordclass.ClassWork {
				t.Errorf("work-class bead %s (type=%s) poured into the graph store", b.ID, b.Type)
			}
		}
	})
}

// oneIDUnreadableStore fails Get for one id with a non-not-found error: a store that
// could not answer, as distinct from one that answered "absent".
type oneIDUnreadableStore struct {
	beads.Store
	failID string
}

func (s oneIDUnreadableStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("store unavailable")
	}
	return s.Store.Get(id)
}

// childListFailingStore fails the list of a bead's DB children: a store that
// could not say which beads name the source as their parent.
type childListFailingStore struct {
	beads.Store
}

func (s childListFailingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.ParentID != "" {
		return nil, errors.New("store unavailable")
	}
	return s.Store.List(query)
}

// Should-fix 4: the attach guard fails closed when it cannot read an
// attachment (it may be live), and stays open for a dangling reference that
// every store answers as absent (the molecule was purged). A read that failed
// is never hidden by another reference the probe did resolve: not by a
// dangling one, nor by a closed attachment the guard skips, and nothing is
// burned past it.
func TestAttachGuardFailsClosedOnAnUnreadableAttachment(t *testing.T) {
	const unreadableID, absentID = "gcg-77", "gcg-404"
	for _, tc := range []struct {
		name string
		// molecule is what the source's molecule_id names: an id the graph
		// store cannot read, an id no store holds, or a molecule in the graph
		// store with this status.
		molecule string
		// workflowUnreadable stamps a workflow_id the graph store cannot read.
		workflowUnreadable bool
		// childListFails fails the list of the source's DB children.
		childListFails bool
		wantRefusal    bool
	}{
		{name: "unreadable molecule_id", molecule: unreadableID, wantRefusal: true},
		{name: "dangling molecule_id", molecule: absentID},
		{name: "dangling molecule_id beside an unreadable workflow_id", molecule: absentID, workflowUnreadable: true, wantRefusal: true},
		{name: "dangling molecule_id beside an unreadable child list", molecule: absentID, childListFails: true, wantRefusal: true},
		{name: "closed molecule beside an unreadable workflow_id", molecule: "closed", workflowUnreadable: true, wantRefusal: true},
		{name: "live molecule beside an unreadable workflow_id", molecule: "open", workflowUnreadable: true, wantRefusal: true},
	} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/batch=%v", tc.name, batch), func(t *testing.T) {
				work, graphLeaf := beads.NewMemStore(), beads.NewMemStore()
				meta := map[string]string{beadmeta.MoleculeIDMetadataKey: tc.molecule}
				attachedID := ""
				if tc.molecule == "open" || tc.molecule == "closed" {
					attached, err := graphLeaf.Create(beads.Bead{Title: "attached molecule", Type: "molecule"})
					if err != nil {
						t.Fatal(err)
					}
					if tc.molecule == "closed" {
						if err := graphLeaf.Close(attached.ID); err != nil {
							t.Fatal(err)
						}
					}
					attachedID = attached.ID
					meta[beadmeta.MoleculeIDMetadataKey] = attachedID
				}
				if tc.workflowUnreadable {
					meta["workflow_id"] = unreadableID
				}
				// An unassigned source: a live attachment is burned to make
				// room unless the guard refuses.
				src, err := work.Create(beads.Bead{Title: "source", Type: "task", Metadata: meta})
				if err != nil {
					t.Fatal(err)
				}
				var q BeadChildQuerier = work
				if tc.childListFails {
					q = childListFailingStore{Store: work}
				}
				scope := attachmentScope{work: work, graph: oneIDUnreadableStore{Store: graphLeaf, failID: unreadableID}}

				var result SlingResult
				if batch {
					err = checkBatchNoMoleculeChildren(q, []beads.Bead{src}, scope, &result, false)
				} else {
					err = checkNoMoleculeChildren(q, src.ID, scope, &result, false)
				}
				if !tc.wantRefusal {
					if err != nil {
						t.Fatalf("guard refused: %v, want no refusal", err)
					}
					return
				}
				var probeErr *AttachmentProbeError
				if !errors.As(err, &probeErr) || !strings.Contains(err.Error(), "could not verify molecule attachment") {
					t.Fatalf("guard: err=%v, want a could-not-verify refusal (*AttachmentProbeError)", err)
				}
				if len(result.AutoBurned) != 0 {
					t.Errorf("guard burned %v past a failed probe", result.AutoBurned)
				}
				if attachedID != "" {
					got, err := graphLeaf.Get(attachedID)
					if err != nil {
						t.Fatal(err)
					}
					if want := map[string]string{"open": "open", "closed": "closed"}[tc.molecule]; got.Status != want {
						t.Errorf("attached molecule status = %q, want %q (the guard must not touch it)", got.Status, want)
					}
				}
			})
		}
	}
}

// Review 6b: a routed bead re-slung with a DIFFERENT --on formula. An
// unassigned source keeps the attach path's burn-and-attach (the stale
// molecule is reported in AutoBurned); a source the target already holds
// fails with MoleculeAttachedError, because attaching would double up on work
// in progress.
func TestSplitSlingDifferentOnFormulaBurnsUnassignedAndRefusesAssigned(t *testing.T) {
	forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
		t.Run("unassigned", func(t *testing.T) {
			src := env.source(t, "")
			first, err := env.sling(t, src.ID, splitAttachVaporA)
			if err != nil {
				t.Fatal(err)
			}
			if err := env.work.SetMetadata(src.ID, beadmeta.RoutedToMetadataKey, "mayor"); err != nil {
				t.Fatal(err)
			}
			second, err := env.sling(t, src.ID, splitAttachVaporB)
			if err != nil {
				t.Fatalf("second --on onto an unassigned source: %v, want burn-and-attach", err)
			}
			if !slices.Contains(second.AutoBurned, first.WispRootID) {
				t.Fatalf("AutoBurned = %v, want the stale molecule %s", second.AutoBurned, first.WispRootID)
			}
		})
		t.Run("assigned", func(t *testing.T) {
			src := env.source(t, "mayor")
			first, err := env.sling(t, src.ID, splitAttachVaporA)
			if err != nil {
				t.Fatal(err)
			}
			if err := env.work.SetMetadata(src.ID, beadmeta.RoutedToMetadataKey, "mayor"); err != nil {
				t.Fatal(err)
			}
			_, err = env.sling(t, src.ID, splitAttachVaporB)
			var molErr *MoleculeAttachedError
			if !errors.As(err, &molErr) || molErr.AttachmentID != first.WispRootID {
				t.Fatalf("second --on onto a held source: err=%v, want MoleculeAttachedError naming %s", err, first.WispRootID)
			}
		})
	})
}

// Review should-fix 2(b): the attached root-only wisp records the one source
// it is attached to, so the wisp GC can ask that source rather than refuse on
// any rig's suspension.
func TestAttachedRootOnlyWispRecordsItsSource(t *testing.T) {
	forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
		src := env.source(t, "")
		res, err := env.sling(t, src.ID, splitAttachVaporA)
		if err != nil {
			t.Fatal(err)
		}
		root, err := env.graph.Get(res.WispRootID)
		if err != nil {
			t.Fatal(err)
		}
		if got := root.Metadata[beadmeta.SourceBeadIDMetadataKey]; got != src.ID {
			t.Errorf("attached root %s gc.source_bead_id=%q, want %q", root.ID, got, src.ID)
		}
		if got := root.Metadata[beadmeta.SourceStoreRefMetadataKey]; got != env.deps.StoreRef {
			t.Errorf("attached root %s gc.source_store_ref=%q, want %q", root.ID, got, env.deps.StoreRef)
		}
	})
}

// Re-review nit: an unassigned source's stale attachment is burned to make
// room for a new formula, but not while one of its steps is in progress: the
// re-sling is refused instead.
func TestSplitSlingDoesNotBurnAnAttachmentWithWorkInProgress(t *testing.T) {
	forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
		src := env.source(t, "")
		first, err := env.sling(t, src.ID, splitAttachPouredA)
		if err != nil {
			t.Fatal(err)
		}
		owner := env.work
		kids, err := owner.Children(first.WispRootID)
		if err != nil || len(kids) == 0 {
			t.Fatalf("children of %s: %v (n=%d)", first.WispRootID, err, len(kids))
		}
		inProgress := "in_progress"
		if err := owner.Update(kids[0].ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatal(err)
		}
		_, err = env.sling(t, src.ID, splitAttachPouredA)
		var molErr *MoleculeAttachedError
		if !errors.As(err, &molErr) {
			t.Fatalf("re-sling over an attachment with a step in progress: err=%v, want MoleculeAttachedError", err)
		}
		root, err := owner.Get(first.WispRootID)
		if err != nil || root.Status == "closed" {
			t.Fatalf("attachment %s was burned while a step was in progress (status=%q err=%v)", first.WispRootID, root.Status, err)
		}
	})
}

// The resolver accepts a formula spelled with its file extension (GitHub
// #3704), so re-slinging the attached formula under that spelling is the same
// request, not a different formula: no conflict, so no burn and no refusal.
func TestConflictingOnFormulaRuleIgnoresTheFileExtension(t *testing.T) {
	molecule := beads.Bead{ID: "gcg-2", Type: "molecule", Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: "first"}}
	for _, spelling := range []string{"first", "first.toml", "first.formula.toml", "first.formula.json"} {
		opts := SlingOpts{BeadOrFormula: "gc-1", OnFormula: spelling}
		if got := conflictingOnFormulaAttachment(opts, molecule); got != nil {
			t.Errorf("--on %s against a molecule poured from first: conflict %v, want none", spelling, got)
		}
	}
	opts := SlingOpts{BeadOrFormula: "gc-1", OnFormula: "second.toml"}
	if got := conflictingOnFormulaAttachment(opts, molecule); got == nil {
		t.Error("--on second.toml against a molecule poured from first: no conflict, want one")
	}
}

// End to end, on both topologies: re-slinging the attached formula spelled
// with its extension is the idempotent retry, whoever holds the source.
func TestSplitSlingSameOnFormulaSpelledWithItsExtensionIsIdempotent(t *testing.T) {
	forEachSlingTopology(t, func(t *testing.T, env splitAttachEnv) {
		for _, assignee := range []string{"", "mayor"} {
			t.Run(fmt.Sprintf("assignee=%q", assignee), func(t *testing.T) {
				src := env.source(t, assignee)
				first, err := env.sling(t, src.ID, splitAttachVaporA)
				if err != nil {
					t.Fatal(err)
				}
				if err := env.work.SetMetadata(src.ID, beadmeta.RoutedToMetadataKey, "mayor"); err != nil {
					t.Fatal(err)
				}
				again, err := env.sling(t, src.ID, splitAttachVaporA+".toml")
				if err != nil {
					t.Fatalf("--on %s.toml after --on %s: %v, want the idempotent retry", splitAttachVaporA, splitAttachVaporA, err)
				}
				if len(again.AutoBurned) != 0 {
					t.Fatalf("--on %s.toml burned %v, want the live molecule kept", splitAttachVaporA, again.AutoBurned)
				}
				live, err := env.graph.Get(first.WispRootID)
				if err != nil {
					t.Fatal(err)
				}
				if live.Status == "closed" {
					t.Fatalf("the attached molecule %s was closed", first.WispRootID)
				}
			})
		}
	})
}

// Re-review M18: the different-formula --on rule covers legacy molecule
// attachments only; a live graph.v2 workflow keeps its own guard.
func TestConflictingOnFormulaRuleSkipsWorkflowAttachments(t *testing.T) {
	opts := SlingOpts{BeadOrFormula: "gc-1", OnFormula: "other"}
	workflow := beads.Bead{ID: "gcg-1", Type: "task", Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		beadmeta.FormulaNameMetadataKey:     "first",
	}}
	if got := conflictingOnFormulaAttachment(opts, workflow); got != nil {
		t.Fatalf("conflictingOnFormulaAttachment on a workflow attachment = %v, want nil", got)
	}
	molecule := beads.Bead{ID: "gcg-2", Type: "molecule", Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: "first"}}
	if got := conflictingOnFormulaAttachment(opts, molecule); got == nil {
		t.Fatal("conflictingOnFormulaAttachment on a legacy molecule with another formula = nil, want a conflict")
	}
}

// listFailingStore fails the subtree read of one root.
type listFailingStore struct {
	beads.Store
	rootID string
}

func (s listFailingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if filters[beadmeta.RootBeadIDMetadataKey] == s.rootID {
		return nil, errors.New("store unavailable")
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

// Re-review round 3: the burn guard fails closed on an unreadable subtree,
// treats a hooked step as active, covers the batch check too, and tells the
// operator how to clear a stale step.
func TestBurnGuardEdges(t *testing.T) {
	mkAttached := func(t *testing.T, stepStatus string) (beads.Store, beads.Bead, beads.Bead) {
		work := beads.NewMemStore()
		root, err := work.Create(beads.Bead{Title: "attached molecule", Type: "molecule"})
		if err != nil {
			t.Fatal(err)
		}
		step, err := work.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
		if err != nil {
			t.Fatal(err)
		}
		if stepStatus != "" {
			if err := work.Update(step.ID, beads.UpdateOpts{Status: &stepStatus}); err != nil {
				t.Fatal(err)
			}
		}
		src, err := work.Create(beads.Bead{Title: "unassigned source", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}})
		if err != nil {
			t.Fatal(err)
		}
		return work, src, root
	}
	t.Run("unreadable-subtree", func(t *testing.T) {
		work, src, root := mkAttached(t, "")
		store := listFailingStore{Store: work, rootID: root.ID}
		var result SlingResult
		err := checkNoMoleculeChildren(store, src.ID, singleStoreAttachmentScope(store), &result, false)
		var molErr *MoleculeAttachedError
		if !errors.As(err, &molErr) || len(result.AutoBurned) != 0 {
			t.Fatalf("burn with an unreadable subtree: err=%v burned=%v, want a refusal", err, result.AutoBurned)
		}
		if !strings.Contains(err.Error(), "subtree could not be read") || strings.Contains(err.Error(), "worker holds "+root.ID) {
			t.Fatalf("refusal does not say the guard could not read the subtree (it must fail closed on that, not on a failed burn): %v", err)
		}
		var probeErr *AttachmentProbeError
		if !errors.As(err, &probeErr) || probeErr.BeadID != src.ID {
			t.Fatalf("refusal over an unreadable subtree = %v, want it to carry an AttachmentProbeError for %s: a store fault, not a conflict", err, src.ID)
		}
	})
	t.Run("hooked-step", func(t *testing.T) {
		work, src, root := mkAttached(t, "hooked")
		var result SlingResult
		err := checkNoMoleculeChildren(work, src.ID, singleStoreAttachmentScope(work), &result, false)
		var molErr *MoleculeAttachedError
		if !errors.As(err, &molErr) {
			t.Fatalf("burn over a hooked step: err=%v, want MoleculeAttachedError", err)
		}
		if !strings.Contains(err.Error(), "--status open") || !strings.Contains(err.Error(), "gc bd close") {
			t.Errorf("refusal does not say how to clear a stale step: %v", err)
		}
		var probeErr *AttachmentProbeError
		if errors.As(err, &probeErr) {
			t.Errorf("a held step is reported as a store fault: %v", err)
		}
		if got, _ := work.Get(root.ID); got.Status == "closed" {
			t.Fatal("the attachment was burned")
		}
	})
	t.Run("batch-unreadable", func(t *testing.T) {
		work, src, root := mkAttached(t, "")
		store := listFailingStore{Store: work, rootID: root.ID}
		var result SlingResult
		err := checkBatchNoMoleculeChildren(store, []beads.Bead{src}, singleStoreAttachmentScope(store), &result, false)
		if err == nil || len(result.AutoBurned) != 0 {
			t.Fatalf("batch burn with an unreadable subtree: err=%v burned=%v, want a refusal", err, result.AutoBurned)
		}
		if !strings.Contains(err.Error(), "subtree could not be read") || strings.Contains(err.Error(), "held by") {
			t.Fatalf("batch refusal misstates an unreadable subtree as held: %v", err)
		}
		var probeErr *AttachmentProbeError
		if !errors.As(err, &probeErr) || probeErr.BeadID != src.ID {
			t.Fatalf("batch refusal over an unreadable subtree = %v, want it to carry an AttachmentProbeError for %s: a store fault, not a conflict", err, src.ID)
		}
	})
	t.Run("batch", func(t *testing.T) {
		work, src, root := mkAttached(t, "in_progress")
		var result SlingResult
		err := checkBatchNoMoleculeChildren(work, []beads.Bead{src}, singleStoreAttachmentScope(work), &result, false)
		if err == nil || len(result.AutoBurned) != 0 {
			t.Fatalf("batch burn over an in-progress step: err=%v burned=%v, want a refusal", err, result.AutoBurned)
		}
		if !strings.Contains(err.Error(), "held by") {
			t.Fatalf("batch refusal does not name the held step: %v", err)
		}
		var probeErr *AttachmentProbeError
		if errors.As(err, &probeErr) {
			t.Errorf("a batch refusal over a held step is reported as a store fault: %v", err)
		}
		if got, _ := work.Get(root.ID); got.Status == "closed" {
			t.Fatal("the batch check burned an attachment a worker holds")
		}
	})
}

// failOnceSubtreeStore fails the FIRST subtree read of one root only: the
// guard's read fails, and a later read (the burn's own) would succeed, as with
// a transient store error.
type failOnceSubtreeStore struct {
	beads.Store
	rootID string
	failed *bool
}

func (s failOnceSubtreeStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if filters[beadmeta.RootBeadIDMetadataKey] == s.rootID && !*s.failed {
		*s.failed = true
		return nil, errors.New("transient: store unavailable")
	}
	return s.Store.ListByMetadata(filters, limit, opts...)
}

// An attachment whose subtree the guard could not read is never burned, at
// either guard site (single and batch), even when the burn's own read would
// succeed: the guard's failed read is what must stop it, not the burn
// failing too.
func TestBurnGuardNeverBurnsAnUnreadableSubtree(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%v", batch), func(t *testing.T) {
			work := beads.NewMemStore()
			root, err := work.Create(beads.Bead{Title: "attached molecule", Type: "molecule"})
			if err != nil {
				t.Fatal(err)
			}
			step, err := work.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
			if err != nil {
				t.Fatal(err)
			}
			ip := "in_progress"
			if err := work.Update(step.ID, beads.UpdateOpts{Status: &ip}); err != nil {
				t.Fatal(err)
			}
			src, err := work.Create(beads.Bead{Title: "unassigned source", Type: "task", Metadata: map[string]string{beadmeta.MoleculeIDMetadataKey: root.ID}})
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			store := failOnceSubtreeStore{Store: work, rootID: root.ID, failed: &failed}
			var result SlingResult
			if batch {
				err = checkBatchNoMoleculeChildren(store, []beads.Bead{src}, singleStoreAttachmentScope(store), &result, false)
			} else {
				err = checkNoMoleculeChildren(store, src.ID, singleStoreAttachmentScope(store), &result, false)
			}
			if !failed {
				t.Fatal("the guard never read the attachment's subtree")
			}
			if err == nil {
				t.Error("the guard passed an attachment whose subtree it could not read")
			}
			gotRoot, _ := work.Get(root.ID)
			gotStep, _ := work.Get(step.ID)
			if gotRoot.Status == "closed" || gotStep.Status == "closed" || len(result.AutoBurned) != 0 {
				t.Errorf("an attachment whose subtree could not be read was burned (root %s, step %s, burned %v)", gotRoot.Status, gotStep.Status, result.AutoBurned)
			}
		})
	}
}
