package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

// Molecule and wisp autoclose across stores (B2).
//
// A v1 POURED recipe compiles to a molecule whose every bead is work class, so
// `gc formula cook`, `gc order run` and the order dispatcher put it on the work
// ledger even on a split city. A graph-class root (a wisp, a graph.v2 workflow)
// lives in the binding. Both autoclose hooks resolved every root through the
// graph-class store only, so on a split city a work-resident v1 molecule never
// autoclosed: its steps closed and the root stayed open.
//
// Both topologies; the split leg is the repo's two-store fixture (strict
// bd-semantics work store, real SQLite binding).

// pourV1Molecule compiles a v1 poured formula with two steps and instantiates
// it in the store its class demands, the way cookOnClassRouted does.
func pourV1Molecule(t *testing.T, e splitEnv, attachTo string) (root beads.Bead, steps []beads.Bead, owner beads.Store) {
	t.Helper()
	dir := t.TempDir()
	name := "split-autoclose-poured"
	body := fmt.Sprintf("formula = %q\nversion = 1\n\n[[steps]]\nid = \"a\"\ntitle = \"Step A\"\n\n[[steps]]\nid = \"b\"\ntitle = \"Step B\"\n", name)
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	recipe, err := formula.Compile(context.Background(), name, []string{dir}, nil)
	if err != nil {
		t.Fatalf("compiling %s: %v", name, err)
	}
	if got := recipeCoordClass(recipe); got != coordclass.ClassWork {
		t.Fatalf("v1 poured recipe classifies %s, want work", got)
	}
	owner = moleculeClassStore(recipe, e.work, e.graphStore())
	res, err := molecule.Instantiate(context.Background(), owner, recipe, molecule.Options{})
	if err != nil {
		t.Fatalf("instantiating %s: %v", name, err)
	}
	root, err = owner.Get(res.RootID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err = owner.Children(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("poured molecule %s has %d children, want 2", root.ID, len(steps))
	}
	if attachTo != "" {
		if err := e.work.SetMetadata(attachTo, beadmeta.MoleculeIDMetadataKey, root.ID); err != nil {
			t.Fatal(err)
		}
	}
	return root, steps, owner
}

func TestMoleculeAutocloseClosesAWorkResidentV1PouredRoot(t *testing.T) {
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
		run := doMoleculeAutocloseWith(e.work, "", events.Discard, steps[len(steps)-1].ID, io.Discard, beads.GraphStore{Store: e.graphStore()})
		got, err := e.work.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("work-resident v1 poured root %s status=%q after both steps closed (run=%+v), want closed", root.ID, got.Status, run)
		}
	})
}

func TestMoleculeAutocloseClosesABindingResidentGraphRoot(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		graph := e.graphStore()
		root, err := graph.Create(beads.Bead{Title: "graph root", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		step, err := graph.Create(beads.Bead{Title: "graph step", Type: "step", ParentID: root.ID, Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.Close(step.ID); err != nil {
			t.Fatal(err)
		}
		doMoleculeAutocloseWith(graph, "", events.Discard, step.ID, io.Discard, beads.GraphStore{Store: graph})
		got, err := graph.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("binding-resident graph root %s status=%q after its step closed, want closed", root.ID, got.Status)
		}
	})
}

// A closed work bead's attached v1 poured molecule (cook --attach puts it on
// the work ledger) is closed by the wisp autoclose hook, in the work store.
func TestWispAutocloseClosesAWorkResidentAttachedMolecule(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		src, err := e.work.Create(beads.Bead{Title: "source", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		root, steps, _ := pourV1Molecule(t, e, src.ID)
		if err := e.work.Close(src.ID); err != nil {
			t.Fatal(err)
		}
		// The attachment is unfinished but abandoned: close one step, leave the
		// other open; a non-parked root with open steps is closed with them.
		if err := e.work.Close(steps[0].ID); err != nil {
			t.Fatal(err)
		}
		if err := e.work.Close(steps[1].ID); err != nil {
			t.Fatal(err)
		}
		run := doWispAutocloseWith(e.work, src.ID, io.Discard, beads.GraphStore{Store: e.graphStore()})
		got, err := e.work.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("attached work-resident molecule %s status=%q after its source %s closed (run=%+v), want closed", root.ID, got.Status, src.ID, run)
		}
	})
}

// The graph-class twin: an attached wisp root in the binding is closed when its
// work source closes.
func TestWispAutocloseClosesABindingResidentAttachedWisp(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		graph := e.graphStore()
		root, err := graph.Create(beads.Bead{Title: "attached wisp", Type: "molecule", Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		src, err := e.work.Create(beads.Bead{Title: "source", Type: "task", Metadata: map[string]string{
			beadmeta.MoleculeIDMetadataKey: root.ID,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.work.Close(src.ID); err != nil {
			t.Fatal(err)
		}
		doWispAutocloseWith(e.work, src.ID, io.Discard, beads.GraphStore{Store: graph})
		got, err := graph.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("attached wisp %s status=%q after its source closed, want closed", root.ID, got.Status)
		}
	})
}

// ga-ty80s.1: the controller's close dispatch hands the autoclose cores the
// CITY graph-class store, but a rig-owned bead's attachments and molecule
// roots live with it in the rig store on a single-store city (the graph store
// a rig sling resolves is the rig store), and in the binding on a split one.
// Both cores fall back from the graph store to the closed bead's own store,
// so either placement is reached.
func TestAutocloseReachesARigOwnedBeadsGraphParents(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		// Where a rig-scope pour lands: the binding on a split city, the rig
		// store itself on a single-store one.
		pourStore := e.rig
		if e.split {
			pourStore = e.graphStore()
		}

		t.Run("wisp-attachment", func(t *testing.T) {
			root, err := pourStore.Create(beads.Bead{Title: "attached wisp", Type: "molecule", Metadata: map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindWisp,
			}})
			if err != nil {
				t.Fatal(err)
			}
			src, err := e.rig.Create(beads.Bead{Title: "rig source", Type: "task", Metadata: map[string]string{
				beadmeta.MoleculeIDMetadataKey: root.ID,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.rig.Close(src.ID); err != nil {
				t.Fatal(err)
			}
			doWispAutocloseWith(e.rig, src.ID, io.Discard, beads.GraphStore{Store: e.graphStore()})
			got, err := pourStore.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" {
				t.Fatalf("rig source's attached wisp %s status=%q after the source closed, want closed", root.ID, got.Status)
			}
		})

		t.Run("molecule-root", func(t *testing.T) {
			root, err := pourStore.Create(beads.Bead{Title: "rig molecule", Type: "molecule", Metadata: map[string]string{
				beadmeta.KindMetadataKey: beadmeta.KindWisp,
			}})
			if err != nil {
				t.Fatal(err)
			}
			step, err := pourStore.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID, Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: root.ID,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := pourStore.Close(step.ID); err != nil {
				t.Fatal(err)
			}
			doMoleculeAutocloseWith(pourStore, "", events.Discard, step.ID, io.Discard, beads.GraphStore{Store: e.graphStore()})
			got, err := pourStore.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "closed" {
				t.Fatalf("rig-placed molecule root %s status=%q after its step closed, want closed", root.ID, got.Status)
			}
		})
	})
}
